package pool

import (
	"context"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// BackoffPolicy 是换号退避的参数。
//
// 解决两个问题：
//  1. 失败后立刻再被选中——同一请求换号会挑到它，别的请求也一样，
//     形成「一直撞同一个坏号 → 一直失败」的密集重试形态；
//  2. 密集重试本身是上游风控最敏感的特征之一。
//
// 与冷却分开：冷却改账号状态（面板显示停用），退避只让调度层跳过一小段时间。
// 403 / 5xx / 网络抖动属于上游波动，不该把号停掉，但也不该零间隔连打。
type BackoffPolicy struct {
	// Enabled 关闭时失败后仍可立即重选（旧行为）。
	Enabled bool
	// Base 是第一次失败的退避时长；之后逐次翻倍，封顶 Max。
	Base time.Duration
	Max  time.Duration
}

// DefaultBackoffPolicy 返回默认值：开启、2 秒起、封顶 60 秒。
//
// 起点故意很短：它只是为了错开「同一瞬间的密集重试」，不是惩罚账号。
func DefaultBackoffPolicy() BackoffPolicy {
	return BackoffPolicy{Enabled: true, Base: 2 * time.Second, Max: 60 * time.Second}
}

// SetBackoffPolicy 热更新退避参数。
func (p *Pool) SetBackoffPolicy(pol BackoffPolicy) {
	if pol.Base <= 0 {
		pol.Base = 2 * time.Second
	}
	if pol.Max <= 0 {
		pol.Max = 60 * time.Second
	}
	if pol.Max < pol.Base {
		pol.Max = pol.Base
	}
	p.mu.Lock()
	p.backoff = pol
	p.mu.Unlock()
}

// BackoffPolicy 返回当前参数。
func (p *Pool) BackoffPolicy() BackoffPolicy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.backoff
}

// MarkBackoff 记一次失败退避。返回本次退避的到期时刻（0 = 未启用）。
//
// 失败链按「上次退避是否已过去很久」判定是否重新计数：一天偶尔失败一次
// 不会让退避时间无限累加。
func (p *Pool) MarkBackoff(ctx context.Context, accID, reason string) int64 {
	pol := p.BackoffPolicy()
	if !pol.Enabled || accID == "" {
		return 0
	}
	now := p.clock().UnixMilli()
	prev, _ := p.st.BackoffOf(ctx, accID)
	hits := int64(0)
	// 上一次退避已经过去超过 Max*10 且没有新失败：视为失败链已断。
	if prev.Hits > 0 && prev.Until > 0 && now-prev.Until <= (pol.Max*10).Milliseconds() {
		hits = prev.Hits
	}
	delay := p.backoffDelay(hits, pol)
	until := now + delay.Milliseconds()
	_ = p.st.UpsertBackoff(ctx, store.AccountBackoff{
		AccountID: accID, Until: until, Reason: reason, Hits: hits + 1,
	})
	return until
}

// ClearBackoff 成功调用后解除退避。
func (p *Pool) ClearBackoff(ctx context.Context, accID string) {
	if accID == "" {
		return
	}
	_ = p.st.ClearBackoff(ctx, accID)
}

// BackoffOf 返回账号当前的退避到期时刻。
func (p *Pool) BackoffOf(ctx context.Context, accID string) int64 {
	b, _ := p.st.BackoffOf(ctx, accID)
	return b.Until
}

// backoffDelay 计算第 hits 次失败（0 基）的退避时长：Base, 2×Base, 4×Base… 封顶 Max。
func (p *Pool) backoffDelay(hits int64, pol BackoffPolicy) time.Duration {
	d := pol.Base
	for i := int64(0); i < hits; i++ {
		if d >= pol.Max {
			break
		}
		d *= 2
		if d > pol.Max {
			d = pol.Max
		}
	}
	return d
}
