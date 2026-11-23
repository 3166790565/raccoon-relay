package pool

import (
	"context"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// LeaseConfig 是单号在途租约的配置。
//
// 为什么需要它：同一个账号被多个请求同时打，上游看到的是「一台设备在并发
// 跑好几段对话」，这是最主要的第三方客户端特征之一。租约让同一账号的并发
// 请求数受控，代价是并发能力下降，所以做成开关与数量都可配。
type LeaseConfig struct {
	// Enabled 关闭时不做任何限制（升级前的行为）。
	Enabled bool
	// Concurrency 是一个账号最多同时持有的租约数，即单号并发上限。
	// 1 = 整号串行（升级前的「单号串行」）；2 = 同一账号允许两路并发，
	// 以此类推。Enabled=false 时本项不生效。
	Concurrency int
	// TTL 是租约有效期。必须大于单次请求的最长耗时，否则长回答跑一半
	// 租约就过期、别的请求插进来。流式回答可能持续数分钟，默认给足。
	TTL time.Duration
}

// DefaultLeaseConfig 返回默认配置：开启、单号 2 路并发。
//
// 2 路是「不至于把同号并发完全放开（那样最容易被判第三方客户端），
// 又比整号串行多一倍吞吐」的折中；面板可改。
func DefaultLeaseConfig() LeaseConfig {
	return LeaseConfig{Enabled: true, Concurrency: 2, TTL: 5 * time.Minute}
}

// LeaseGrant 是一次租约占用的凭据。释放与续期都要带上它，
// 因为同一个账号可能有多个槽位同时被本进程占用。
type LeaseGrant struct {
	AccountID string
	// Slot 是占到的槽号；-1 表示租约未启用、这次没有真正占槽。
	Slot int
}

type leaseState struct {
	until int64 // 当前槽位写进库里的到期时刻（本进程写的值，续期/释放 CAS 用）
}

// SetLeaseConfig 热更新租约配置。
func (p *Pool) SetLeaseConfig(c LeaseConfig) {
	if c.TTL <= 0 {
		c.TTL = 5 * time.Minute
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	p.mu.Lock()
	p.lease = c
	if p.held == nil {
		p.held = map[string]map[int]*leaseState{}
	}
	p.mu.Unlock()
}

// LeaseConfig 返回当前配置。
func (p *Pool) LeaseConfig() LeaseConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lease
}

// AcquireLease 为已经选中的账号抢占一个在途租约槽位。
//
// 返回 (grant, ok)。未启用时直接 ok=true、grant.Slot=-1（不落库）。
// 抢占失败说明该账号的并发槽位已满，调用方应当换号或等待。
func (p *Pool) AcquireLease(ctx context.Context, acc *store.Account) (*LeaseGrant, bool) {
	cfg := p.LeaseConfig()
	if acc == nil {
		return nil, false
	}
	if !cfg.Enabled {
		return &LeaseGrant{AccountID: acc.ID, Slot: -1}, true
	}
	now := p.clock().UnixMilli()
	slot, until, ok, err := p.st.AcquireLeaseSlot(ctx, acc.ID, cfg.Concurrency, cfg.TTL.Milliseconds(), now)
	if err != nil || !ok {
		return nil, false
	}
	p.mu.Lock()
	if p.held == nil {
		p.held = map[string]map[int]*leaseState{}
	}
	if p.held[acc.ID] == nil {
		p.held[acc.ID] = map[int]*leaseState{}
	}
	p.held[acc.ID][slot] = &leaseState{until: until}
	p.mu.Unlock()
	acc.LeaseUntil = until
	return &LeaseGrant{AccountID: acc.ID, Slot: slot}, true
}

// ReleaseAccountLease 释放一次租约占用。
//
// 走 CAS：只释放自己写进去的那个槽位与到期时刻。若槽位已被别人接管
// （TTL 过期后他人抢占），这里不会把别人的租约一起清掉。
func (p *Pool) ReleaseAccountLease(ctx context.Context, acc *store.Account, grant *LeaseGrant) {
	if acc == nil || grant == nil || grant.Slot < 0 {
		return
	}
	p.mu.Lock()
	st := (*leaseState)(nil)
	if slots := p.held[acc.ID]; slots != nil {
		st = slots[grant.Slot]
		delete(slots, grant.Slot)
		if len(slots) == 0 {
			delete(p.held, acc.ID)
		}
	}
	enabled := p.lease.Enabled
	p.mu.Unlock()
	if st == nil || !enabled {
		return
	}
	_ = p.st.ReleaseLeaseSlot(ctx, acc.ID, grant.Slot, st.until)
}

// RenewAccountLease 续期一次租约占用，供流式写出的过程中按节流调用。
//
// 返回 false 表示已经失去独占（槽位过期被他人接管）。流式请求此时仍可继续
// 读自己的响应，但下一次写库/选号不该再假设自己独占这个槽位。
func (p *Pool) RenewAccountLease(ctx context.Context, acc *store.Account, grant *LeaseGrant) bool {
	if acc == nil || grant == nil || grant.Slot < 0 {
		return true
	}
	cfg := p.LeaseConfig()
	if !cfg.Enabled {
		return true
	}
	p.mu.Lock()
	st := (*leaseState)(nil)
	if slots := p.held[acc.ID]; slots != nil {
		st = slots[grant.Slot]
	}
	p.mu.Unlock()
	if st == nil {
		return false
	}
	now := p.clock().UnixMilli()
	until, err := p.st.RenewLeaseSlot(ctx, acc.ID, grant.Slot, cfg.TTL.Milliseconds(), st.until, now)
	if err != nil || until == 0 {
		return false
	}
	p.mu.Lock()
	if cur := p.held[acc.ID][grant.Slot]; cur != nil {
		cur.until = until
	}
	p.mu.Unlock()
	acc.LeaseUntil = until
	return true
}

// ForceClearLease 强制清掉账号的全部残租约（面板手动解除用）。
func (p *Pool) ForceClearLease(ctx context.Context, accID string) error {
	p.mu.Lock()
	delete(p.held, accID)
	p.mu.Unlock()
	return p.st.ClearLeaseSlots(ctx, accID)
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
