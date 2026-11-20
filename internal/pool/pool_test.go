package pool

import (
	"context"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addAcc(t *testing.T, st *store.Store, uid string, credits int64, opts ...func(*store.Account)) *store.Account {
	t.Helper()
	a := &store.Account{
		UID: uid, Name: uid, Token: "tok", Status: store.StatusOnline,
		Credits: credits, CreditsAt: time.Now().UnixMilli(), Enabled: true,
	}
	for _, o := range opts {
		o(a)
	}
	if err := st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNormalizeStrategy(t *testing.T) {
	cases := map[string]string{
		"load_balance": StrategyLoadBalance,
		"round_robin":  StrategyRoundRobin,
		"optimal":      StrategyLoadBalance, // 旧默认值
		"credit_first": StrategyLoadBalance,
		"manual":       StrategyLoadBalance,
		"expire_first": StrategyLoadBalance,
		"":             StrategyLoadBalance,
	}
	for in, want := range cases {
		got, ok := NormalizeStrategy(in)
		if !ok {
			t.Fatalf("策略 %q 应被接受（退化为默认），实际被拒绝", in)
		}
		if got != want {
			t.Fatalf("策略 %q 应归一为 %s，实际 %s", in, want, got)
		}
	}
	// 真正无法识别的值交给调用方退回默认
	if _, ok := NormalizeStrategy("round-robin"); ok {
		t.Fatal("无法识别的策略应返回 false")
	}
}

func TestRoundRobinRotates(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyRoundRobin, cool)
	addAcc(t, st, "a", 1)
	addAcc(t, st, "b", 1)
	addAcc(t, st, "c", 1)

	ctx := context.Background()
	// 连续选 6 次，每次都立刻释放：轮询应完整转两圈，而不是反复选同一个。
	seq := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		lease, err := p.Pick(ctx, "", map[string]bool{})
		if err != nil || lease == nil {
			t.Fatal(err)
		}
		seq = append(seq, lease.Account.UID)
		p.ReleaseLease(lease)
	}
	seen := map[string]bool{}
	for _, uid := range seq {
		seen[uid] = true
	}
	if len(seen) != 3 {
		t.Fatalf("轮询应覆盖三个账号，实际 %v", seq)
	}
	for i := 1; i < len(seq); i++ {
		if seq[i] == seq[i-1] {
			t.Fatalf("轮询不该连续选中同一个号: %v", seq)
		}
	}
}

func TestLoadBalancePrefersIdleAccount(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	addAcc(t, st, "a", 100)
	addAcc(t, st, "b", 100)

	ctx := context.Background()
	first, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || first == nil {
		t.Fatal(err)
	}
	// 第一个请求还没释放（在途 1），下一次应挑另一个空闲账号
	second, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || second == nil {
		t.Fatal(err)
	}
	if second.Account.UID == first.Account.UID {
		t.Fatalf("负载均衡应避开有在途请求的账号，两次都选了 %s", first.Account.UID)
	}
	p.ReleaseLease(first)
	p.ReleaseLease(second)

	// 两个都在途过又都释放后，按最久未用继续轮转
	third, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || third == nil {
		t.Fatal(err)
	}
	p.ReleaseLease(third)
}

func TestPickSkipsIneligible(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	addAcc(t, st, "cooling", 100, func(a *store.Account) {
		a.Status = store.StatusCooling
		a.CoolUntil = time.Now().Add(time.Hour).UnixMilli()
	})
	addAcc(t, st, "no-token", 100, func(a *store.Account) { a.Token = "" })
	addAcc(t, st, "zero", 0)
	addAcc(t, st, "ok", 50)
	addAcc(t, st, "disabled", 100, func(a *store.Account) { a.Enabled = false })

	lease, err := p.Pick(context.Background(), "", map[string]bool{})
	if err != nil || lease == nil {
		t.Fatalf("应选出唯一可用账号: %v", err)
	}
	if lease.Account.UID != "ok" {
		t.Fatalf("应选 ok，实际 %s", lease.Account.UID)
	}
}

func TestPickHonorsExclude(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	addAcc(t, st, "a", 900)
	addAcc(t, st, "b", 100)

	lease, err := p.Pick(context.Background(), "", map[string]bool{getID(t, st, "a"): true})
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if lease.Account.UID != "b" {
		t.Fatalf("排除 a 后应选 b，实际 %s", lease.Account.UID)
	}
}

func TestPickReturnsNilWhenEmpty(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	addAcc(t, st, "only", 100, func(a *store.Account) { a.Status = store.StatusRelogin })
	lease, err := p.Pick(context.Background(), "", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if lease != nil {
		t.Fatal("全池不可用时应返回 nil")
	}
}

func TestModelCooldownFiltering(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	addAcc(t, st, "a", 900)
	addAcc(t, st, "b", 100)

	ctx := context.Background()
	cool.CoolModel(ctx, getID(t, st, "a"), "sn-glm-5-3", "模型限流")

	lease, err := p.Pick(ctx, "sn-glm-5-3", map[string]bool{})
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if lease.Account.UID != "b" {
		t.Fatalf("模型冷却应让 a 被跳过，实际 %s", lease.Account.UID)
	}
	p.ReleaseLease(lease)

	// 换模型后 a 仍可用：429 不该把整个账号停掉
	lease2, _ := p.Pick(ctx, "other-model", map[string]bool{})
	if lease2 == nil || lease2.Account.UID != "a" {
		t.Fatalf("换模型后 a 应恢复可选，实际 %v", lease2)
	}
	p.ReleaseLease(lease2)

	// 账号本身状态没变：模型冷却不动 status
	acc, _ := st.GetByUID(ctx, "a")
	if acc.Status != store.StatusOnline {
		t.Fatalf("模型级冷却不应改账号状态，实际 %s", acc.Status)
	}
}

func TestModelCooldownLadder(t *testing.T) {
	cases := []struct {
		hits int64
		want time.Duration
	}{
		{0, 3 * time.Minute},
		{1, 5 * time.Minute},
		{2, 10 * time.Minute},
		{3, 20 * time.Minute},
		{4, 40 * time.Minute},
		{5, 80 * time.Minute},
		{20, 24 * time.Hour},
	}
	for _, c := range cases {
		if got := modelCoolDelay(c.hits); got != c.want {
			t.Fatalf("第 %d 次命中应为 %s，实际 %s", c.hits+1, c.want, got)
		}
	}
}

// 同账号同模型连续 429：3 分钟 → 5 分钟 → 10 分钟；成功一次后清零。
func TestCoolModelEscalatesAndResets(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()
	a := addAcc(t, st, "a", 100)
	model := "sn-test"

	check := func(want time.Duration) {
		t.Helper()
		cool.mu.Lock()
		m := cool.models[coolKey(a.ID, model)]
		cool.mu.Unlock()
		got := time.Duration(m.until-time.Now().UnixMilli()) * time.Millisecond
		// 允许少量调度误差
		if got < want-2*time.Second || got > want+2*time.Second {
			t.Fatalf("冷却应为约 %s，实际 %s", want, got)
		}
	}

	cool.CoolModel(ctx, a.ID, model, "429")
	check(3 * time.Minute)
	cool.CoolModel(ctx, a.ID, model, "429")
	check(5 * time.Minute)
	cool.CoolModel(ctx, a.ID, model, "429")
	check(10 * time.Minute)

	cool.ClearModel(ctx, a.ID, model)
	if cool.IsModelCooled(a.ID, model) {
		t.Fatal("成功调用后应清零模型冷却")
	}
	cool.CoolModel(ctx, a.ID, model, "429")
	check(3 * time.Minute) // 清零后重新从 3 分钟开始

	rows, err := st.ListModelCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("库里应只剩一条模型冷却，实际 %d", len(rows))
	}
}

// 冷却到期后再失败必须接着上一档，而不是从 3 分钟重来。
// 选号时 IsModelCooled 每轮都会被调用，它不能顺手把记录删掉——否则阶梯永远停在一档。
func TestModelCooldownEscalatesAfterExpiry(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()
	a := addAcc(t, st, "a", 100)
	model := "m-escalate"

	cool.CoolModel(ctx, a.ID, model, "429") // 第 1 档 3 分钟
	cool.mu.Lock()
	cool.models[coolKey(a.ID, model)] = modelCool{until: time.Now().Add(-time.Second).UnixMilli(), hits: 1}
	cool.mu.Unlock()

	// 模拟真实的选号路径：遇到已到期的记录
	if cool.IsModelCooled(a.ID, model) {
		t.Fatal("已到期的冷却不应再拦选号")
	}
	// 到期后再 429：应接第二档 5 分钟
	cool.CoolModel(ctx, a.ID, model, "429")
	cool.mu.Lock()
	m := cool.models[coolKey(a.ID, model)]
	cool.mu.Unlock()
	left := time.Duration(m.until-time.Now().UnixMilli()) * time.Millisecond
	if m.hits != 2 || left < 4*time.Minute || left > 6*time.Minute {
		t.Fatalf("到期后再失败应接第二档（5 分钟, hits=2），实际 hits=%d left=%s", m.hits, left)
	}
}

// 402 写入的 exhausted 带次日 04:00 的到期时刻，到点应由 ReviveExpired 放回在线；
// 而 CoolUntil=0 的低积分暂停不能被它复活（那是 ApplyCreditPolicy 的事）。
func TestReviveExpiredHandlesExhaustedWithDeadline(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()
	a := addAcc(t, st, "quota", 0)

	past := time.Now().Add(-time.Minute).UnixMilli()
	exhausted := store.StatusExhausted
	_ = st.UpdateAccount(ctx, a.ID, store.AccountPatch{Status: &exhausted, CoolUntil: &past})

	cool.ReviveExpired(ctx)

	got, _ := st.GetAccount(ctx, a.ID)
	if got.Status != store.StatusOnline {
		t.Fatalf("402 耗尽到期后应复活为 online，实际 %s", got.Status)
	}

	// 未来的到期时刻不能提前放
	future := time.Now().Add(time.Hour).UnixMilli()
	_ = st.UpdateAccount(ctx, a.ID, store.AccountPatch{Status: &exhausted, CoolUntil: &future})
	cool.ReviveExpired(ctx)
	got, _ = st.GetAccount(ctx, a.ID)
	if got.Status != store.StatusExhausted {
		t.Fatalf("未到期的 402 耗尽不该复活，实际 %s", got.Status)
	}
}

// 冷却早已结束且长期没再失败：失败链重置，不该无限累加冷却时长。
func TestCoolModelResetsAfterIdle(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()
	a := addAcc(t, st, "a", 100)

	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	cool.mu.Lock()
	cool.models[coolKey(a.ID, "m")] = modelCool{until: old, hits: 4}
	cool.mu.Unlock()

	cool.CoolModel(ctx, a.ID, "m", "429")
	cool.mu.Lock()
	m := cool.models[coolKey(a.ID, "m")]
	cool.mu.Unlock()
	if m.hits != 1 {
		t.Fatalf("空闲超过 24h 后应从 1 档重新开始，实际 hits=%d", m.hits)
	}
}

func TestModelCooldownDisabledFallsThrough(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, false)
	p := New(st, StrategyLoadBalance, cool)
	addAcc(t, st, "a", 100)

	ctx := context.Background()
	cool.CoolModel(ctx, getID(t, st, "a"), "sn-test", "429")
	lease, err := p.Pick(ctx, "sn-test", map[string]bool{})
	if err != nil || lease == nil {
		t.Fatalf("关闭模型级冷却时不该过滤账号: %v", err)
	}
	p.ReleaseLease(lease)
}

func TestCreditPolicyPausesAndResumes(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	ctx := context.Background()

	low := addAcc(t, st, "low", 5)
	high := addAcc(t, st, "high", 5000)

	p.SetCreditPolicy(CreditPolicy{Enabled: true, StopBelow: 100, ResumeAbove: 200})
	p.ApplyCreditPolicy(ctx)

	lease, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if lease.Account.UID != "high" {
		t.Fatalf("低积分号应被暂停，实际选中 %s", lease.Account.UID)
	}
	p.ReleaseLease(lease)

	got, _ := st.GetAccount(ctx, low.ID)
	if got.Status != store.StatusExhausted || got.CoolUntil != 0 {
		t.Fatalf("低积分应进入 exhausted 且无固定到期时间，实际 %s/%d", got.Status, got.CoolUntil)
	}
	if gotHigh, _ := st.GetAccount(ctx, high.ID); gotHigh.Status != store.StatusOnline {
		t.Fatalf("高积分号应保持在线，实际 %s", gotHigh.Status)
	}

	// 签到补上积分 → 立刻解除，不必等任何冷却
	c := int64(300)
	_ = st.UpdateAccount(ctx, low.ID, store.AccountPatch{Credits: &c})
	p.ApplyCreditPolicy(ctx)
	got, _ = st.GetAccount(ctx, low.ID)
	if got.Status != store.StatusOnline || got.CoolReason != "" {
		t.Fatalf("积分回升后应恢复在线，实际 %s/%q", got.Status, got.CoolReason)
	}
}

// 402 的 exhausted 带未来的 CoolUntil，不能被积分规则提前放出来。
func TestCreditResumeDoesNotOverrideHardQuota(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	ctx := context.Background()

	a := addAcc(t, st, "quota", 5000)
	until := time.Now().Add(6 * time.Hour).UnixMilli()
	exhausted := store.StatusExhausted
	_ = st.UpdateAccount(ctx, a.ID, store.AccountPatch{Status: &exhausted, CoolUntil: &until})

	p.SetCreditPolicy(CreditPolicy{Enabled: true, StopBelow: 100, ResumeAbove: 200})
	p.ApplyCreditPolicy(ctx)

	got, _ := st.GetAccount(ctx, a.ID)
	if got.Status != store.StatusExhausted {
		t.Fatalf("402 的硬耗尽不该被积分规则解除，实际 %s", got.Status)
	}
}

func TestCreditPolicyDisabledKeepsUnknownCredits(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	addAcc(t, st, "unknown", -1)
	if lease, err := p.Pick(context.Background(), "", map[string]bool{}); err != nil || lease == nil {
		t.Fatalf("未知余额（-1）不该被当成低积分: %v", err)
	}
}

func TestCooldownRevive(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()
	a := addAcc(t, st, "x", 100)

	past := time.Now().Add(-time.Minute).UnixMilli()
	cooling := store.StatusCooling
	_ = st.UpdateAccount(ctx, a.ID, store.AccountPatch{Status: &cooling, CoolUntil: &past})

	cool.ReviveExpired(ctx)

	got, _ := st.GetAccount(ctx, a.ID)
	if got.Status != store.StatusOnline {
		t.Fatalf("冷却到期应复活为 online，实际 %s", got.Status)
	}
}

// exhausted 且 CoolUntil=0 是「等积分恢复」，巡检不能把它当到期冷却放出来。
func TestReviveSkipsCreditPause(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()
	a := addAcc(t, st, "low", 5)
	exhausted := store.StatusExhausted
	zero := int64(0)
	_ = st.UpdateAccount(ctx, a.ID, store.AccountPatch{Status: &exhausted, CoolUntil: &zero})

	cool.ReviveExpired(ctx)
	got, _ := st.GetAccount(ctx, a.ID)
	if got.Status != store.StatusExhausted {
		t.Fatalf("低积分暂停不该被复活，实际 %s", got.Status)
	}
}

func TestCooldownClassification(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()
	a := addAcc(t, st, "x", 100)

	cool.Cool(ctx, a.ID, FailRate, "")
	got, _ := st.GetAccount(ctx, a.ID)
	if got.Status != store.StatusCooling {
		t.Fatalf("429 应进入 cooling，实际 %s", got.Status)
	}
	delta := got.CoolUntil - time.Now().UnixMilli()
	if delta < 50_000 || delta > 70_000 {
		t.Fatalf("429 冷却应约 60s，实际 %dms", delta)
	}

	cool.Cool(ctx, a.ID, FailRelogin, "")
	got, _ = st.GetAccount(ctx, a.ID)
	if got.Status != store.StatusRelogin {
		t.Fatalf("401 应进入 relogin，实际 %s", got.Status)
	}
}

func getID(t *testing.T, st *store.Store, uid string) string {
	t.Helper()
	a, err := st.GetByUID(context.Background(), uid)
	if err != nil || a == nil {
		t.Fatalf("找不到账号 %s", uid)
	}
	return a.ID
}
