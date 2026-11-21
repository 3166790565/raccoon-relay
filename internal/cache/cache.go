// Package cache 提供跨实例协调所需的最小能力：
//   - 分布式锁：后台任务（签到、冷却巡检、清理、续期）在多个副本间只跑一份；
//   - 简单 KV 缓存：API Key 校验结果等可短时容忍过期的小块数据。
//
// Redis 不可用时自动降级为进程内实现：
// 单实例行为与改造前完全一致，多副本下退化为「各自运行」，但不会把服务拖垮。
package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// 所有 Redis 调用都要带超时：Redis 抖动不能拖慢转发路径。
const opTimeout = 400 * time.Millisecond

// Client 是对 Redis 的薄封装。URL 为空或连接失败时 Enabled() 为 false，
// 此时锁恒获取成功、缓存只在本进程内有效。
type Client struct {
	rdb    *redis.Client
	prefix string
	redis  bool

	mu   sync.Mutex
	kv   map[string]localItem
	lock map[string]time.Time
}

type localItem struct {
	val string
	exp time.Time
}

// New 创建客户端。redisURL 为空时直接进入本地模式。
func New(redisURL, prefix string) *Client {
	if prefix == "" {
		prefix = "raccoon:"
	}
	c := &Client{prefix: prefix, kv: map[string]localItem{}, lock: map[string]time.Time{}}
	if redisURL == "" {
		return c
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Printf("[缓存] REDIS_URL 无法解析，降级为本地实现: %v", err)
		return c
	}
	c.rdb = redis.NewClient(opts)
	return c
}

// Enabled 表示当前是否真的连着 Redis。
func (c *Client) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.redis
}

// Start 启动探测。required 为 true 时，Redis 连不上返回错误（由调用方中止启动）；
// 否则只告警并降级。之后每 30s 再试一次，支持 Redis 后启动。
func (c *Client) Start(ctx context.Context, required bool) error {
	if c.rdb == nil {
		return nil
	}
	if err := c.pingErr(ctx); err != nil && required {
		return err
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = c.pingErr(ctx)
			}
		}
	}()
	return nil
}

func (c *Client) pingErr(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	was := c.Enabled()
	err := c.rdb.Ping(pctx).Err()
	c.mu.Lock()
	c.redis = err == nil
	c.mu.Unlock()
	switch {
	case err != nil && was:
		log.Printf("[缓存] Redis 连接断开，降级为本地实现: %v", err)
	case err == nil && !was:
		log.Printf("[缓存] Redis 已连接")
	case err != nil:
		log.Printf("[缓存] Redis 不可用，降级为本地实现: %v", err)
	}
	return err
}

// Lock 尝试获取名为 name 的锁，返回释放函数。
// ok=false 表示别的实例正持有。
//
// 本地模式下恒返回 ok=true，与改造前「只有一个实例」的假设一致。
func (c *Client) Lock(ctx context.Context, name string, ttl time.Duration) (release func(), ok bool) {
	if !c.Enabled() {
		return c.localLock(name, ttl)
	}
	token := c.prefix + "lock:" + name + ":" + randomToken()
	key := c.prefix + "lock:" + name
	pctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	got, err := c.rdb.SetNX(pctx, key, token, ttl).Result()
	if err != nil || !got {
		return func() {}, false
	}

	// 后台续期：任务耗时可能超过 TTL（签到逐号有间隔），
	// 每 ttl/3 在 token 仍匹配时延长。
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(ttl / 3)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				rctx, rcancel := context.WithTimeout(context.Background(), opTimeout)
				_ = renewScript.Run(rctx, c.rdb, []string{key}, token, ttl.Milliseconds()).Err()
				rcancel()
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			// 释放必须用独立 ctx：任务 ctx 可能已经取消。
			rctx, rcancel := context.WithTimeout(context.Background(), opTimeout)
			defer rcancel()
			_ = releaseScript.Run(rctx, c.rdb, []string{key}, token).Err()
		})
	}, true
}

func (c *Client) localLock(name string, ttl time.Duration) (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if exp, ok := c.lock[name]; ok && time.Now().Before(exp) {
		return func() {}, false
	}
	c.lock[name] = time.Now().Add(ttl)
	return func() {
		c.mu.Lock()
		delete(c.lock, name)
		c.mu.Unlock()
	}, true
}

// Get 读取缓存。miss 返回空串与 false。
func (c *Client) Get(ctx context.Context, key string) (string, bool) {
	full := c.prefix + "kv:" + key
	if c.Enabled() {
		pctx, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		v, err := c.rdb.Get(pctx, full).Result()
		if err == nil {
			return v, true
		}
		if !errors.Is(err, redis.Nil) {
			return "", false
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.kv[full]
	if !ok || time.Now().After(it.exp) {
		delete(c.kv, full)
		return "", false
	}
	return it.val, true
}

// Set 写入缓存，ttl 内有效。
func (c *Client) Set(ctx context.Context, key, val string, ttl time.Duration) {
	full := c.prefix + "kv:" + key
	if c.Enabled() {
		pctx, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		if err := c.rdb.Set(pctx, full, val, ttl).Err(); err == nil {
			return
		}
	}
	c.mu.Lock()
	c.kv[full] = localItem{val: val, exp: time.Now().Add(ttl)}
	c.mu.Unlock()
}

// Del 删除缓存条目（删除 Key 时主动失效）。
func (c *Client) Del(ctx context.Context, key string) {
	full := c.prefix + "kv:" + key
	if c.Enabled() {
		pctx, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		_ = c.rdb.Del(pctx, full).Err()
	}
	c.mu.Lock()
	delete(c.kv, full)
	c.mu.Unlock()
}

// 只有 token 匹配才续期/删除，避免误删别人拿到的同名锁。
var renewScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0`)

var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0`)

// randomToken 生成锁令牌，避免 A 释放掉 B 持有的同名锁。
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}
