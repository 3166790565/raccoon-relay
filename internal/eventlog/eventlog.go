// Package eventlog 是运行事件的异步写入器。
//
// 为什么不让调用方直接写库：
//   - SQLite 这里只开了一个连接，转发热路径上每来一条事件就写一次库，
//     遇到 429/5xx 风暴会把写锁排满，反过来拖慢正常请求；
//   - 同一条事件短时间内反复出现（同一个号同一个模型连续 429）没有必要各写一行，
//     内存里先按 key 聚合并累加次数，再定期合并落库。
//
// 设计：Emit 只做 O(1) 的 map 累加，绝不阻塞；后台每 flushInterval 把桶里的
// 聚合结果写成一行（次数带上），库那边还有一次 10 分钟窗口的二次合并兜底。
package eventlog

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

const (
	flushInterval = 2 * time.Second
	maxBuckets    = 500 // 内存桶上限，防止异常场景把 map 撑爆
)

type key struct {
	level, scope, component, accountID, message string
}

type bucket struct {
	ev    store.RuntimeEvent
	count int64
}

// Recorder 把事件聚合后写入 runtime_events。
type Recorder struct {
	st *store.Store

	mu      sync.Mutex
	buckets map[key]*bucket
	dropped int64
}

func New(st *store.Store) *Recorder {
	return &Recorder{st: st, buckets: map[key]*bucket{}}
}

// Emit 记录一条事件。实现为不阻塞：内存满时丢弃 info 级事件，warn/error 仍保留。
func (r *Recorder) Emit(ev store.RuntimeEvent) {
	if r == nil {
		return
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	ev.Level = store.NormalizeLevel(ev.Level)
	if ev.Scope == "" {
		ev.Scope = store.ScopeSystem
	}
	if ev.Message == "" {
		return
	}

	k := key{ev.Level, ev.Scope, ev.Component, ev.AccountID, ev.Message}
	r.mu.Lock()
	if b, ok := r.buckets[k]; ok {
		b.count++
		b.ev.Detail = ev.Detail
		r.mu.Unlock()
		return
	}
	if len(r.buckets) >= maxBuckets {
		// 桶满了：优先丢 info，warn/error 换个方式挤进去（丢掉最旧的一条 info 也行，
		// 但实现上简单起见直接计入 dropped，flush 时打一行提示）。
		if ev.Level == store.LevelInfo {
			r.dropped++
			r.mu.Unlock()
			return
		}
	}
	r.buckets[k] = &bucket{ev: ev, count: 1}
	r.mu.Unlock()
}

// Start 启动后台 flush 循环，直到 ctx 取消。取消时做最后一次落库。
func (r *Recorder) Start(ctx context.Context) {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.Flush(context.Background())
			return
		case <-t.C:
			r.Flush(ctx)
		}
	}
}

// Flush 把当前聚合结果写入数据库。失败的事件会被丢弃（日志本身不该影响主流程），
// 但在标准错误里留一行，方便排查「为什么面板上看不到日志」。
func (r *Recorder) Flush(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if len(r.buckets) == 0 {
		r.dropped = 0
		r.mu.Unlock()
		return
	}
	pending := r.buckets
	r.buckets = map[key]*bucket{}
	dropped := r.dropped
	r.dropped = 0
	r.mu.Unlock()

	var failed int
	for _, b := range pending {
		ev := b.ev
		ev.Count = b.count
		if err := r.st.AddEvent(ctx, ev); err != nil {
			failed++
		}
	}
	if failed > 0 {
		log.Printf("[运行日志] %d 条事件写入失败", failed)
	}
	if dropped > 0 {
		log.Printf("[运行日志] 事件积压，丢弃 %d 条 info 级事件", dropped)
	}
}
