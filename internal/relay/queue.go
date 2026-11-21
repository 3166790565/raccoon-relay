package relay

import (
	"context"
	"log"
	"time"

	"github.com/3166790565/raccoon-relay/internal/cache"
)

// 并发闸门：限制同时打到上游的请求数。
//
// 号池容量有限，超出时不要直接 429——那会让客户端看到一次失败的请求。
// 这里让多余的请求排队，等前面的请求结束释放槽位后再继续。
//
// 未配置（MaxConcurrent <= 0）时完全不影响现有行为。
const (
	// 槽位 TTL：进程崩溃没来得及释放时，Redis 侧按这个时间自动回收。
	// 必须大于单次请求的最长耗时（流式请求可能持续数分钟），
	// 否则还在跑的请求会被误判为“已结束”。
	relaySlotTTL = 30 * time.Minute
	// 排队多久还没拿到槽位就放弃。默认 10 分钟，通常流式请求也不会超过。
	defaultQueueWait = 10 * time.Minute
)

// SetSemaphore 注入跨实例并发闸门。传 nil 关闭。
func (s *Server) SetSemaphore(sem cache.Semaphore) {
	s.sem = sem
}

// SetConcurrencyLimit 热更新并发上限与排队时长，面板保存后立即生效。
// limit <= 0 表示不限制。
func (s *Server) SetConcurrencyLimit(limit, waitSec int) {
	s.cfg.MaxConcurrent = limit
	s.cfg.QueueWaitSec = waitSec
}

// ConcurrencyLimit 返回当前上限与排队时长，供面板展示。
func (s *Server) ConcurrencyLimit() (limit, waitSec int) {
	return s.cfg.MaxConcurrent, s.cfg.QueueWaitSec
}

// acquireSlot 尝试占用一个并发槽位；关闭或未配置时直接放行。
//
// 返回的 release 必须 defer 调用。客户端在排队期间断开时返回 ctx.Err()，
// 由调用方转成 499，不会再打上游。
func (s *Server) acquireSlot(ctx context.Context, api, model string) (func(), error) {
	limit := s.cfg.MaxConcurrent
	if limit <= 0 || s.sem == nil {
		return func() {}, nil
	}
	wait := time.Duration(s.cfg.QueueWaitSec) * time.Second
	if wait <= 0 {
		wait = defaultQueueWait
	}

	start := time.Now()
	// 排队本身也受客户端生命周期约束：ctx 取消就不再等。
	qctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	release, err := s.sem.Acquire(qctx, limit, relaySlotTTL)
	if err != nil {
		// 排队截止时间先到 = 我们的准入控制超时；否则是客户端自己断开了。
		if qctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return func() {}, errQueueTimeout
		}
		if ctx.Err() != nil {
			return func() {}, ctx.Err()
		}
		return func() {}, errQueueTimeout
	}
	if waited := time.Since(start); waited > time.Second {
		log.Printf("[并发] %s 请求排队 %.1fs 后获得槽位（上限 %d）", api, waited.Seconds(), limit)
	}
	return release, nil
}

var errQueueTimeout = &queueTimeoutError{}

type queueTimeoutError struct{}

func (*queueTimeoutError) Error() string { return "请求排队超时，请稍后重试" }

// isQueueTimeout 判断错误是否来自排队超时。
func isQueueTimeout(err error) bool {
	_, ok := err.(*queueTimeoutError)
	return ok
}