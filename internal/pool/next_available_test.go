package pool

import (
	"context"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// NextAvailableIn 只在「号在线、只是暂时不可选」时给出等待时间：
// 在途租约、退避窗口、并发上限打满都算；号不在线或模型级冷却则不算。
func TestNextAvailableInBusyAccount(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetLeaseConfig(LeaseConfig{Enabled: true, Concurrency: 1, TTL: 5 * time.Minute})
	a := addAcc(t, st, "a", 100)

	ctx := context.Background()
	grant, ok := p.AcquireLease(ctx, a)
	if !ok {
		t.Fatal("空租约应能抢到")
	}
	defer p.ReleaseAccountLease(ctx, a, grant)

	wait, ok := p.NextAvailableIn(ctx, "", "", map[string]bool{})
	if !ok {
		t.Fatal("账号在线、只是租约被占，应给出等待时间")
	}
	if wait < 4*time.Minute {
		t.Fatalf("应等到租约到期附近，实际 %v", wait)
	}
}

func TestNextAvailableInBackoff(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetBackoffPolicy(BackoffPolicy{Enabled: true, Base: time.Minute, Max: time.Minute})
	a := addAcc(t, st, "a", 100)

	ctx := context.Background()
	p.MarkBackoff(ctx, a.ID, "上游 502")

	wait, ok := p.NextAvailableIn(ctx, "", "", map[string]bool{})
	if !ok || wait <= 0 {
		t.Fatalf("退避中的在线号应给出等待时间，实际 %v/%v", wait, ok)
	}
}

func TestNextAvailableInNoOnlineAccount(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	a := addAcc(t, st, "a", 100)
	ctx := context.Background()
	cool.Cool(ctx, a.ID, FailQuota, "积分耗尽")

	if _, ok := p.NextAvailableIn(ctx, "", "", map[string]bool{}); ok {
		t.Fatal("号已冷却时不该让调用方等待")
	}
}

// 长时间并发上限打满、没有明确的租约/退避到期时刻时，也应允许短轮询重试。
func TestNextAvailableInMaxConcurrent(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	addAcc(t, st, "a", 100, func(x *store.Account) { x.MaxConcurrent = 1 })

	ctx := context.Background()
	lease, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || lease == nil {
		t.Fatalf("首个请求应能选到号：%v", err)
	}
	defer p.ReleaseLease(lease)

	wait, ok := p.NextAvailableIn(ctx, "", "", map[string]bool{})
	if !ok || wait != 0 {
		t.Fatalf("并发打满时应允许立即轮询重试，实际 %v/%v", wait, ok)
	}
}

// 不同供应商的号不能互相等待：日日新的请求不该等小浣熊的号。
func TestNextAvailableInProviderFilter(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetLeaseConfig(LeaseConfig{Enabled: true, Concurrency: 1, TTL: 5 * time.Minute})
	raccoonAcc := addAcc(t, st, "r", 100)
	snAcc := addAcc(t, st, "s", 100, func(x *store.Account) { x.Provider = store.ProviderSenseNova })

	ctx := context.Background()
	grant, ok := p.AcquireLease(ctx, raccoonAcc)
	if !ok {
		t.Fatal("空租约应能抢到")
	}
	defer p.ReleaseAccountLease(ctx, raccoonAcc, grant)

	// 日日新请求：snAcc 空闲可选，立刻返回 0/true，不受小浣熊租约影响。
	wait, ok := p.NextAvailableIn(ctx, "", store.ProviderSenseNova, map[string]bool{})
	if !ok || wait != 0 {
		t.Fatalf("日日新应立刻可选，实际 %v/%v", wait, ok)
	}
	// 小浣熊请求：唯一在线号被占，给出等待时间。
	wait, ok = p.NextAvailableIn(ctx, "", store.ProviderRaccoon, map[string]bool{})
	if !ok || wait <= 0 {
		t.Fatalf("小浣熊应等待被占用的号，实际 %v/%v", wait, ok)
	}
	_ = snAcc
}
