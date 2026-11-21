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

// CoolQuota：恢复了签到计划就停到计划时刻；没有计划才回退到次日 04:00。
func TestCoolQuotaUsesResumeFunc(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()
	a := addAcc(t, st, "quota", 0)

	want := time.Now().Add(3 * time.Hour).Truncate(time.Second).UnixMilli()
	cool.SetQuotaResume(func(_ context.Context, acc *store.Account) (int64, bool) {
		if acc.ID != a.ID {
			t.Fatalf("应收到被冷却的账号，实际 %s", acc.ID)
		}
		return want, true
	})
	got := cool.CoolQuota(ctx, a, "积分余额不足")
	if got != want {
		t.Fatalf("应停在签到计划时刻 %d，实际 %d", want, got)
	}
	acc, _ := st.GetAccount(ctx, a.ID)
	if acc.Status != store.StatusExhausted || acc.CoolUntil != want {
		t.Fatalf("账号应 exhausted 且带计划到期时刻，实际 %s/%d", acc.Status, acc.CoolUntil)
	}

	// 没有可靠计划时回退到次日 04:00，而不是立刻放行。
	b := addAcc(t, st, "quota2", 0)
	cool.SetQuotaResume(func(context.Context, *store.Account) (int64, bool) { return 0, false })
	fallback := cool.CoolQuota(ctx, b, "积分余额不足")
	if fallback <= time.Now().UnixMilli() {
		t.Fatalf("回退时刻必须在未来，实际 %d", fallback)
	}
	accB, _ := st.GetAccount(ctx, b.ID)
	if accB.Status != store.StatusExhausted {
		t.Fatalf("回退时也应判积分耗尽，实际 %s", accB.Status)
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

// 面板「限流」列依赖 ModelCooldownsOf：只列仍在冷却中的组合，
// 已到期的记录（失败链历史）不能被当成「限流中」显示。
func TestModelCooldownsOfForPanel(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()
	a := addAcc(t, st, "a", 100)
	b := addAcc(t, st, "b", 100)

	cool.CoolModel(ctx, a.ID, "sn-glm-5-3", "429 限流")
	cool.CoolModel(ctx, a.ID, "sn-deepseek", "429 限流")
	cool.CoolModel(ctx, b.ID, "sn-glm-5-3", "429 限流")

	got := cool.ModelCooldownsOf(a.ID)
	if len(got) != 2 {
		t.Fatalf("a 应有 2 条限流明细，实际 %d", len(got))
	}
	// 展示用的是原始大小写，不是内存 key 里的小写
	seen := map[string]bool{}
	for _, m := range got {
		seen[m.Model] = true
		if m.Until <= time.Now().UnixMilli() {
			t.Fatalf("%s 应仍在冷却中", m.Model)
		}
	}
	if !seen["sn-glm-5-3"] || !seen["sn-deepseek"] {
		t.Fatalf("模型名应与入库时一致，实际 %v", seen)
	}
	if n := len(cool.ModelCooldownsOf(b.ID)); n != 1 {
		t.Fatalf("b 只应有 1 条，实际 %d", n)
	}

	// 人为把一条设置成已过期：它只是失败链历史，不该再出现在面板上
	cool.mu.Lock()
	cool.models[coolKey(a.ID, "sn-deepseek")] = modelCool{
		accID: a.ID, model: "sn-deepseek", until: time.Now().Add(-time.Minute).UnixMilli(), hits: 1,
	}
	cool.mu.Unlock()
	if n := len(cool.ModelCooldownsOf(a.ID)); n != 1 {
		t.Fatalf("已到期的记录不应计入限流，实际剩 %d 条", n)
	}

	// 手动清除后立即消失
	cool.ClearModelNow(ctx, a.ID, "sn-glm-5-3")
	if n := len(cool.ModelCooldownsOf(a.ID)); n != 0 {
		t.Fatalf("清除后不应再有明细，实际 %d", n)
	}
}

// 重启（Hydrate）之后面板仍要看得到限流明细，否则面板会把冷却中的号显示成「正常」。
func TestModelCooldownsOfAfterHydrate(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	a := addAcc(t, st, "a", 100)
	NewCooldownBook(st, true).CoolModel(ctx, a.ID, "sn-glm-5-3", "429 限流")

	fresh := NewCooldownBook(st, true)
	fresh.Hydrate(ctx)
	got := fresh.ModelCooldownsOf(a.ID)
	if len(got) != 1 || got[0].Model != "sn-glm-5-3" {
		t.Fatalf("重启后应恢复 1 条限流明细，实际 %+v", got)
	}
	if got[0].Reason != "429 限流" {
		t.Fatalf("重启后应保留原因，实际 %q", got[0].Reason)
	}
}

// 单号并发上限：某号在途达到上限后，Pick 应跳过它、落到其它号上。
func TestPickSkipsAccountAtMaxConcurrent(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	ctx := context.Background()

	// a 上限 1，b 不限。给 a 更高优先级，确保正常情况下它会被优先选中。
	addAcc(t, st, "a", 100, func(x *store.Account) { x.MaxConcurrent = 1 })
	addAcc(t, st, "b", 1)

	first, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.Account.UID != "a" {
		t.Fatalf("第一次应选到 a，实际 %+v", first)
	}
	// 不释放 a，再选一次：a 已满载，应落到 b。
	second, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.Account.UID != "b" {
		t.Fatalf("a 满载后应选到 b，实际 %+v", second)
	}

	// 释放 a（同时释放 b），a 应重新可选。
	p.ReleaseLease(first)
	p.ReleaseLease(second)

	// 两个号都空载，但 a 余额更高，优先选 a——说明上限只在满载时生效，
	// 不会把这个号永久排除。
	third, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if third == nil || third.Account.UID != "a" {
		t.Fatalf("释放后 a 应重新可选，实际 %+v", third)
	}
}

// 全部号都满载时返回 nil，让上层排队/换号，而不是越限。
func TestPickReturnsNilWhenAllFull(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	ctx := context.Background()
	addAcc(t, st, "a", 1, func(x *store.Account) { x.MaxConcurrent = 1 })

	lease, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil || lease == nil {
		t.Fatalf("第一次应选中: %v", err)
	}
	got, err := p.Pick(ctx, "", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("全部满载应返回 nil，实际选中 %s", got.Account.UID)
	}
}
