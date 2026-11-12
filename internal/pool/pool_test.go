package pool

import (
	"context"
	"testing"
	"time"

	"raccoon-relay/internal/store"
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

func TestCreditFirstPicksRichest(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, "credit_first", cool)
	addAcc(t, st, "poor", 10)
	addAcc(t, st, "rich", 9000)
	addAcc(t, st, "mid", 500)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		lease, err := p.Pick(ctx, "", map[string]bool{})
		if err != nil || lease == nil {
			t.Fatalf("选号失败: %v", err)
		}
		if lease.Account.UID != "rich" {
			t.Fatalf("应选积分最高的 rich，实际 %s", lease.Account.UID)
		}
		p.ReleaseLease(lease)
	}
}

func TestRoundRobinRotates(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, "round_robin", cool)
	addAcc(t, st, "a", 1)
	addAcc(t, st, "b", 1)

	ctx := context.Background()
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		lease, err := p.Pick(ctx, "", map[string]bool{})
		if err != nil || lease == nil {
			t.Fatal(err)
		}
		seen[lease.Account.UID] = true
		p.ReleaseLease(lease)
	}
	if len(seen) != 2 {
		t.Fatalf("轮询应覆盖两个账号，实际 %v", seen)
	}
}

func TestPickSkipsIneligible(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, "optimal", cool)
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
	p := New(st, "credit_first", cool)
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
	p := New(st, "optimal", cool)
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
	p := New(st, "credit_first", cool)
	addAcc(t, st, "a", 900)
	addAcc(t, st, "b", 100)

	ctx := context.Background()
	cool.CoolModel(ctx, getID(t, st, "a"), "sn-glm-5-3", "模型限流", time.Minute)

	lease, err := p.Pick(ctx, "sn-glm-5-3", map[string]bool{})
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	if lease.Account.UID != "b" {
		t.Fatalf("模型冷却应让 a 被跳过，实际 %s", lease.Account.UID)
	}

	// 换模型后 a 仍可用
	lease2, _ := p.Pick(ctx, "other-model", map[string]bool{})
	if lease2.Account.UID != "a" {
		t.Fatalf("换模型后 a 应恢复可选，实际 %s", lease2.Account.UID)
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
