package pool

import (
	"context"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// LeaseConfig 是单号串行（在途租约）的配置。
//
// 为什么需要它：同一个账号被多个请求同时打，上游看到的是「一台设备在并发
// 跑好几段对话」，这是最主要的第三方客户端特征之一。租约让同一账号的请求
// 串行排队，代价是并发能力下降，所以做成开关。
type LeaseConfig struct {
	Enabled bool
	// TTL 是租约有效期。必须大于单次请求的最长耗时，否则长回答跑一半
	// 租约就过期、别的请求插进来。流式回答可能持续数分钟，默认给足。
	TTL time.Duration
}

// DefaultLeaseConfig 返回默认配置：关闭，保持升级前的行为。
func DefaultLeaseConfig() LeaseConfig {
	return LeaseConfig{Enabled: false, TTL: 5 * time.Minute}
}

type leaseState struct {
	until int64 // 当前租约到期时刻（本进程写入库的值，续期 CAS 用）
}

// SetLeaseConfig 热更新租约配置。
func (p *Pool) SetLeaseConfig(c LeaseConfig) {
	if c.TTL <= 0 {
		c.TTL = 5 * time.Minute
	}
	p.mu.Lock()
	p.lease = c
	if p.held == nil {
		p.held = map[string]*leaseState{}
	}
	p.mu.Unlock()
}

// LeaseConfig 返回当前配置。
func (p *Pool) LeaseConfig() LeaseConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lease
}

// AcquireLease 为已经选中的账号抢占在途租约。
//
// 返回 (until, ok)。未启用时直接 ok=true、until=0（不落库）。
// 抢占失败说明别的请求刚拿到这个号，调用方应当换号或等待。
func (p *Pool) AcquireLease(ctx context.Context, acc *store.Account) (int64, bool) {
	cfg := p.LeaseConfig()
	if !cfg.Enabled || acc == nil {
		return 0, true
	}
	now := p.clock().UnixMilli()
	until, err := p.st.AcquireLease(ctx, acc.ID, cfg.TTL.Milliseconds(), now)
	if err != nil || until == 0 {
		return 0, false
	}
	p.mu.Lock()
	if p.held == nil {
		p.held = map[string]*leaseState{}
	}
	p.held[acc.ID] = &leaseState{until: until}
	p.mu.Unlock()
	acc.LeaseUntil = until
	return until, true
}

// ReleaseLease 释放本进程持有的租约。
//
// 走 CAS：只释放自己写进去的那个到期时刻。若租约已被别人抢走（TTL 过期后
// 他人接管），这里不会把别人的租约一起清掉。
func (p *Pool) ReleaseAccountLease(ctx context.Context, acc *store.Account) {
	if acc == nil {
		return
	}
	p.mu.Lock()
	st, ok := p.held[acc.ID]
	delete(p.held, acc.ID)
	enabled := p.lease.Enabled
	p.mu.Unlock()
	if !ok || !enabled {
		return
	}
	_ = p.st.ReleaseLease(ctx, acc.ID, st.until)
}

// RenewLease 续期本进程持有的租约，供流式写出的过程中按节流调用。
//
// 返回 false 表示已经失去独占（租约过期被他人接管）。流式请求此时仍可继续
// 读自己的响应，但下一次写库/选号不该再假设自己独占这个号。
func (p *Pool) RenewAccountLease(ctx context.Context, acc *store.Account) bool {
	if acc == nil {
		return false
	}
	cfg := p.LeaseConfig()
	if !cfg.Enabled {
		return true
	}
	p.mu.Lock()
	st, ok := p.held[acc.ID]
	p.mu.Unlock()
	if !ok {
		return false
	}
	now := p.clock().UnixMilli()
	until, err := p.st.RenewLease(ctx, acc.ID, cfg.TTL.Milliseconds(), st.until, now)
	if err != nil || until == 0 {
		return false
	}
	p.mu.Lock()
	if cur, ok := p.held[acc.ID]; ok {
		cur.until = until
	}
	p.mu.Unlock()
	acc.LeaseUntil = until
	return true
}

// ForceClearLease 强制清掉账号残租约（面板手动解除用）。
func (p *Pool) ForceClearLease(ctx context.Context, accID string) error {
	p.mu.Lock()
	delete(p.held, accID)
	p.mu.Unlock()
	return p.st.ReleaseLease(ctx, accID, 0)
}

// HeldLeases 返回本进程当前持有的账号 id，供面板/排障展示。
func (p *Pool) HeldLeases() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.held))
	for id := range p.held {
		out = append(out, id)
	}
	return out
}
