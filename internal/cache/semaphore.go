package cache

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Semaphore 是跨实例的并发闸门。
//
// 用于"请求超过号池承载就排队"的场景：拿到槽位才继续转发，拿不到就在
// Acquire 里等，直到有请求结束释放，或等待超时。
//
// 语义是「最多 limit 个同时在运行」，而不是「最多排队 limit 个」。
// 实例内的 goroutine 各自持有一个编号 slot，释放时按编号归还，
// 编号重复不会导致计数漂移。
type Semaphore interface {
	Acquire(ctx context.Context, limit int, ttl time.Duration) (release func(), err error)
	Active(ctx context.Context) (int64, error)
	Enabled() bool
}

// NewSemaphore 返回队列实现：Redis 可用时跨实例共享计数，
// 否则退化为进程内计数（单实例行为不变）。
func (c *Client) NewSemaphore(name string) Semaphore {
	if c.rdb == nil {
		return &localSemaphore{name: name}
	}
	return &redisSemaphore{c: c, name: name}
}

// ---------- Redis 实现 ----------

type redisSemaphore struct {
	c    *Client
	name string
}

// acquireScript 原子地尝试占用一个槽位：
//   - 先清掉已经过期的持有者（进程崩溃没释放），
//   - 仍有空位就写入自己的 slot。
//
// 返回 -1 表示已满。
var acquireScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local slot = ARGV[4]
redis.call("zremrangebyscore", KEYS[1], "-inf", now)
local n = redis.call("zcard", KEYS[1])
if n >= limit then
	return -1
end
redis.call("zadd", KEYS[1], now + ttl, slot)
return n + 1`)

func (s *redisSemaphore) Acquire(ctx context.Context, limit int, ttl time.Duration) (func(), error) {
	cutover := time.Now()
	for {
		slot := randomToken()
		now := time.Now().UnixMilli()
		pctx, cancel := context.WithTimeout(ctx, opTimeout)
		got, err := acquireScript.Run(pctx, s.c.rdb,
			[]string{s.c.prefix + "sem:" + s.name},
			now, limit, ttl.Milliseconds(), slot,
		).Int64()
		cancel()
		if err != nil {
			// Redis 抖动时不能把请求卡死：记一行日志后放行，
			// 宁可短暂超出上限，也不要整站挂起。
			log.Printf("[并发] 获取 Redis 槽位失败，本次放行: %v", err)
			return func() {}, nil
		}
		if got >= 0 {
			var once sync.Once
			return func() {
				once.Do(func() {
					rctx, rcancel := context.WithTimeout(context.Background(), opTimeout)
					defer rcancel()
					_ = s.c.rdb.ZRem(rctx, s.c.prefix+"sem:"+s.name, slot).Err()
				})
			}, nil
		}
		// 已满：退避后重试。退避带抖动，避免所有排队请求同时唤醒。
		wait := 40*time.Millisecond + time.Duration(cutover.UnixNano()%37)*time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (s *redisSemaphore) Active(ctx context.Context) (int64, error) {
	pctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	key := s.c.prefix + "sem:" + s.name
	_ = s.c.rdb.ZRemRangeByScore(pctx, key, "-inf", strconv.FormatInt(time.Now().UnixMilli(), 10)).Err()
	return s.c.rdb.ZCard(pctx, key).Result()
}

func (s *redisSemaphore) Enabled() bool { return true }

// ---------- 本地实现 ----------

type localSemaphore struct {
	name string
	mu   sync.Mutex
	n    int
}

// Acquire 用短轮询等待空位。sync.Cond 不感知 ctx，取消时容易永久阻塞；
// 这个场景排队时长以秒计，轮询足够且实现简单。
func (s *localSemaphore) Acquire(ctx context.Context, limit int, _ time.Duration) (func(), error) {
	for {
		s.mu.Lock()
		if s.n < limit {
			s.n++
			s.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					s.mu.Lock()
					if s.n > 0 {
						s.n--
					}
					s.mu.Unlock()
				})
			}, nil
		}
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func (s *localSemaphore) Active(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(s.n), nil
}

func (s *localSemaphore) Enabled() bool { return false }