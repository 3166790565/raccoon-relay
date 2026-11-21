package pool

import (
	"context"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 号池全在冷却里时，Pick 返回 nil，但 PickProbe 要挑出最早恢复的那个号，
// 让调用方能拿到上游的真实错误，而不是我们推断出来的「没有可用账号」。
func TestPickProbeChoosesEarliestRecovery(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	p := New(st, StrategyLoadBalance, cool)
	ctx := context.Background()

	soon := addAcc(t, st, "soon", 100, func(a *store.Account) {
		a.Status, a.CoolUntil = store.StatusCooling, time.Now().Add(1*time.Minute).UnixMilli()
	})
	later := addAcc(t, st, "later", 100, func(a *store.Account) {
		a.Status, a.CoolUntil = store.StatusCooling, time.Now().Add(10*time.Minute).UnixMilli()
	})
	_ = later

	if lease, _ := p.Pick(ctx, "", map[string]bool{}); lease != nil {
		t.Fatal("全部冷却时 Pick 不应返回账号")
	}
	probe := p.PickProbe(ctx, "", map[string]bool{})
	if probe == nil {
		t.Fatal("PickProbe 应挑出一个可探测的号")
	}
	if probe.Account.ID != soon.ID {
		t.Fatalf("应挑最早恢复的号，实际 %s", probe.Account.Name)
	}
	p.ReleaseLease(probe)

	// 停用或需重登的号不参与探测
	_ = st.UpdateAccount(ctx, soon.ID, store.AccountPatch{Enabled: boolPtr(false)})
	_ = st.UpdateAccount(ctx, later.ID, store.AccountPatch{Status: strPtr(store.StatusRelogin)})
	if probe := p.PickProbe(ctx, "", map[string]bool{}); probe != nil {
		t.Fatalf("停用/需重登的号不该被探测，实际 %s", probe.Account.Name)
	}
}

func boolPtr(v bool) *bool    { return &v }
func strPtr(v string) *string { return &v }
