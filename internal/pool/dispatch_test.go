package pool

import (
	"context"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// ---------- 退避 ----------

// 失败后退避窗口内不再被选中，窗口过期后恢复。
func TestBackoffSkipsAccountUntilExpired(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetBackoffPolicy(BackoffPolicy{Enabled: true, Base: time.Minute, Max: time.Minute})
	a := addAcc(t, st, "a", 100)
	b := addAcc(t, st, "b", 100)

	ctx := context.Background()
	until := p.MarkBackoff(ctx, a.ID, "上游 502")
	if until <= time.Now().UnixMilli() {
		t.Fatalf("退避到期时刻应大于当前时间，实际 %d", until)
	}

	// 只有两个号，a 在退避中 → 只能选到 b。
	for i := 0; i < 3; i++ {
		lease, err := p.Pick(ctx, "", map[string]bool{})
		if err != nil || lease == nil {
			t.Fatal(err)
		}
		if lease.Account.ID != b.ID {
			t.Fatalf("a 在退避中不该被选中，实际选了 %s", lease.Account.UID)
		}
		p.ReleaseLease(lease)
	}

	// 手动把退避推到过去，a 应重新可被选中。
	if err := st.UpsertBackoff(ctx, store.AccountBackoff{AccountID: a.ID, Until: time.Now().UnixMilli() - 1}); err != nil {
		t.Fatal(err)
	}
	p.SetStrategy(StrategyRoundRobin)
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		lease, err := p.Pick(ctx, "", map[string]bool{})
		if err != nil || lease == nil {
			t.Fatal(err)
		}
		seen[lease.Account.ID] = true
		p.ReleaseLease(lease)
	}
	if !seen[a.ID] {
		t.Fatal("退避过期后 a 应重新参与调度")
	}
}

// 退避时长逐个翻倍并封顶。
func TestBackoffDelayDoublesAndCaps(t *testing.T) {
	p := New(newStore(t), StrategyLoadBalance, NewCooldownBook(newStore(t), true))
	pol := BackoffPolicy{Enabled: true, Base: 2 * time.Second, Max: 10 * time.Second}
	want := []time.Duration{2, 4, 8, 10, 10}
	for i, exp := range want {
		got := p.backoffDelay(int64(i), pol)
		if got != exp*time.Second {
			t.Fatalf("第 %d 次退避应为 %s，实际 %s", i+1, exp*time.Second, got)
		}
	}
}

// 关闭退避后失败不影响选号（旧行为）。
func TestBackoffDisabledKeepsAccountSelectable(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetBackoffPolicy(BackoffPolicy{Enabled: false})
	a := addAcc(t, st, "a", 100)

	ctx := context.Background()
	if until := p.MarkBackoff(ctx, a.ID, "x"); until != 0 {
		t.Fatalf("关闭后退避不应写入，实际 %d", until)
	}
	lease, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || lease == nil || lease.Account.ID != a.ID {
		t.Fatal("关闭退避后账号应仍可被选中")
	}
}

// ---------- 在途租约 ----------

// 抢到租约的号在另一次选号里被跳过；释放后恢复。
func TestLeaseBlocksOtherRequests(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetLeaseConfig(LeaseConfig{Enabled: true, Concurrency: 1, TTL: time.Minute})
	a := addAcc(t, st, "a", 100)
	b := addAcc(t, st, "b", 100)

	ctx := context.Background()
	grant, ok := p.AcquireLease(ctx, a)
	if !ok {
		t.Fatal("空租约应能抢到")
	}

	for i := 0; i < 3; i++ {
		lease, err := p.Pick(ctx, "", map[string]bool{})
		if err != nil || lease == nil {
			t.Fatal(err)
		}
		if lease.Account.ID != b.ID {
			t.Fatalf("a 持有租约时不该被选中，实际 %s", lease.Account.UID)
		}
		p.ReleaseLease(lease)
	}

	p.ReleaseAccountLease(ctx, a, grant)
	lease, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if lease.Account.ID != a.ID && lease.Account.ID != b.ID {
		t.Fatalf("释放后两个号都可选，实际 %s", lease.Account.UID)
	}
}

// 同一账号在并发数为 1 时只能被一个请求抢到租约。
func TestLeaseIsExclusive(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetLeaseConfig(LeaseConfig{Enabled: true, Concurrency: 1, TTL: time.Minute})
	a := addAcc(t, st, "a", 100)

	ctx := context.Background()
	if _, ok := p.AcquireLease(ctx, a); !ok {
		t.Fatal("首次抢租约应成功")
	}
	if _, ok := p.AcquireLease(ctx, a); ok {
		t.Fatal("并发数为 1 时第二次抢租约应失败")
	}
}

// 并发数设为 N 时，同一账号最多同时持有 N 个租约，第 N+1 个被拒。
func TestLeaseConcurrencyN(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetLeaseConfig(LeaseConfig{Enabled: true, Concurrency: 3, TTL: time.Minute})
	a := addAcc(t, st, "a", 100)

	ctx := context.Background()
	slots := map[int]bool{}
	for i := 0; i < 3; i++ {
		g, ok := p.AcquireLease(ctx, a)
		if !ok {
			t.Fatalf("第 %d 个租约应成功", i+1)
		}
		if slots[g.Slot] {
			t.Fatalf("槽号 %d 被重复发放", g.Slot)
		}
		slots[g.Slot] = true
	}
	if _, ok := p.AcquireLease(ctx, a); ok {
		t.Fatal("第 4 个租约应被拒绝")
	}
}

// 关闭租约时并发不受限（升级前的行为）。
func TestLeaseDisabledAllowsConcurrency(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetLeaseConfig(LeaseConfig{Enabled: false})
	a := addAcc(t, st, "a", 100)

	ctx := context.Background()
	if _, ok := p.AcquireLease(ctx, a); !ok {
		t.Fatal("关闭租约时应直接放行")
	}
	if _, ok := p.AcquireLease(ctx, a); !ok {
		t.Fatal("关闭租约时应允许重复占用")
	}
}

// 租约到期后自动失效，不需要任何人手动释放（进程崩溃场景）。
func TestLeaseExpiresAutomatically(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	p.SetLeaseConfig(LeaseConfig{Enabled: true, Concurrency: 1, TTL: time.Second})
	a := addAcc(t, st, "a", 100)

	ctx := context.Background()
	if _, ok := p.AcquireLease(ctx, a); !ok {
		t.Fatal("应能抢到租约")
	}
	// 直接把库里的槽位推到过去，模拟「进程崩溃没释放、TTL 已过」。
	_ = st.ReleaseLeaseSlot(ctx, a.ID, 0, 0)
	p.mu.Lock()
	delete(p.held, a.ID) // 本进程也忘掉，模拟重启后的新进程
	p.mu.Unlock()

	lease, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || lease == nil {
		t.Fatalf("过期租约不该继续占号: %v", err)
	}
	if lease.Account.ID != a.ID {
		t.Fatalf("过期租约后应选回 a，实际 %s", lease.Account.UID)
	}
}

// ---------- 会话粘性偏好 ----------

// 设置优先账号后，即使负载均衡本会选别人，也应先选它。
func TestPreferAccountWins(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	a := addAcc(t, st, "a", 1)
	b := addAcc(t, st, "b", 1000) // 积分远多于 a，正常会优先被选

	ctx := context.Background()
	hints := PickHints{PreferID: b.ID}
	lease, err := p.PickProviderWithHints(ctx, "", "", map[string]bool{}, hints)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if lease.Account.ID != b.ID {
		t.Fatalf("应优先选中绑定的号，实际 %s", lease.Account.UID)
	}
	p.ReleaseLease(lease)

	// 绑定被排除（比如刚失败）时应回落到正常策略，而不是卡住。
	lease, err = p.PickProviderWithHints(ctx, "", "", map[string]bool{b.ID: true}, hints)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if lease.Account.ID != a.ID {
		t.Fatalf("绑定的号被排除后应选另一个号，实际 %s", lease.Account.UID)
	}
}

// 优先账号不在线/冷却时，选号不能卡住。
func TestPreferIgnoresUnavailableAccount(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	a := addAcc(t, st, "a", 100)
	off := addAcc(t, st, "off", 100, func(acc *store.Account) { acc.Status = store.StatusCooling })

	ctx := context.Background()
	lease, err := p.PickProviderWithHints(ctx, "", "", map[string]bool{}, PickHints{PreferID: off.ID})
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if lease.Account.ID != a.ID {
		t.Fatalf("冷却中的绑定号应被跳过，实际 %s", lease.Account.UID)
	}
}

// 并发请求各自带自己的绑定号：提示必须互不覆盖。
// 这正是不把它放进 Pool 字段的原因——池级字段下 A 会拿到 B 的号。
func TestPickHintsArePerCallNotShared(t *testing.T) {
	st := newStore(t)
	p := New(st, StrategyLoadBalance, NewCooldownBook(st, true))
	a := addAcc(t, st, "a", 1000)
	b := addAcc(t, st, "b", 1000)

	ctx := context.Background()
	// 交错地各选一次，中间不留任何「清空优先号」的调用。
	la, err := p.PickProviderWithHints(ctx, "", "", map[string]bool{}, PickHints{PreferID: a.ID})
	if err != nil || la == nil {
		t.Fatal(err)
	}
	if la.Account.ID != a.ID {
		t.Fatalf("A 应拿到自己的绑定号 a，实际 %s", la.Account.UID)
	}
	lb, err := p.PickProviderWithHints(ctx, "", "", map[string]bool{}, PickHints{PreferID: b.ID})
	if err != nil || lb == nil {
		t.Fatal(err)
	}
	if lb.Account.ID != b.ID {
		t.Fatalf("B 应拿到自己的绑定号 b，实际 %s", lb.Account.UID)
	}
	// 不带提示的请求按正常策略走，不受上一次提示影响。
	ln, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || ln == nil {
		t.Fatal(err)
	}
	if ln.Account.ID != a.ID && ln.Account.ID != b.ID {
		t.Fatalf("无提示时应在池内正常选号，实际 %s", ln.Account.UID)
	}
}

func ptrInt64(v int64) *int64 { return &v }
