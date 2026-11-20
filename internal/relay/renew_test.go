package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// agent2api 导入的号没有 refresh_token：续期必须回退用 access_token，
// 否则这类号到期只能重登。
func TestRefreshFallsBackToAccessToken(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	acc := &store.Account{
		UID: "pu-a2a", Name: "a2a", Token: testToken("pu-a2a"),
		Status: store.StatusOnline, Enabled: true,
		TokenExp: time.Now().Add(30 * 24 * time.Hour).UnixMilli(),
	}
	if err := h.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	if _, err := h.engine.Refresher.RefreshNow(ctx, acc); err != nil {
		t.Fatalf("无 refreshToken 时应回退用 access 续期，实际失败: %v", err)
	}
	h.up.mu.Lock()
	refreshes := h.up.refreshes
	h.up.mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("应调用刷新端点 1 次，实际 %d", refreshes)
	}
	got, _ := h.st.GetByUID(ctx, "pu-a2a")
	if got.Token != "new-access-token" {
		t.Fatalf("刷新后的 token 未落库: %s", got.Token)
	}
}

// 续期成功后，被误判成 relogin 的号应自动恢复在线。
func TestRenewRecoversRelogin(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	acc := &store.Account{
		UID: "pu-dead", Name: "dead", Token: testToken("pu-dead"), RefreshToken: "rf",
		Status: store.StatusRelogin, CoolReason: "凭证失效", Enabled: true,
		TokenExp: time.Now().Add(time.Hour).UnixMilli(),
	}
	if err := h.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	res := h.engine.Refresher.RenewDue(ctx, 24*time.Hour, 0)
	if res.Checked != 1 || res.Renewed != 1 || res.Recovered != 1 {
		t.Fatalf("续期统计不对: %+v", res)
	}
	got, _ := h.st.GetAccount(ctx, acc.ID)
	if got.Status != store.StatusOnline || got.CoolReason != "" {
		t.Fatalf("续期成功应恢复 online，实际 %s / %q", got.Status, got.CoolReason)
	}
}

// 只续临期号：还在有效期深处的不动，避免提前轮换凭据。
func TestRenewOnlyTouchesNearExpiry(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	now := time.Now()
	for _, a := range []*store.Account{
		{UID: "pu-fresh", Name: "fresh", Token: testToken("pu-fresh"), RefreshToken: "rf",
			Status: store.StatusOnline, Enabled: true, TokenExp: now.Add(20 * 24 * time.Hour).UnixMilli()},
		{UID: "pu-due", Name: "due", Token: testToken("pu-due"), RefreshToken: "rf",
			Status: store.StatusOnline, Enabled: true, TokenExp: now.Add(2 * 24 * time.Hour).UnixMilli()},
	} {
		if err := h.st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	res := h.engine.Refresher.RenewDue(ctx, 3*24*time.Hour, 0)
	if res.Checked != 1 || res.Renewed != 1 {
		t.Fatalf("只应续临期的那一个: %+v", res)
	}
	fresh, _ := h.st.GetByUID(ctx, "pu-fresh")
	due, _ := h.st.GetByUID(ctx, "pu-due")
	if fresh.Token != testToken("pu-fresh") {
		t.Fatal("未临期的号不该被刷新")
	}
	if due.Token != "new-access-token" {
		t.Fatal("临期号应已续期")
	}
}

// 续期被上游 401 拒绝时，把号标成 relogin 供面板清理。
func TestRenewMarksDeadCredentialRelogin(t *testing.T) {
	h := newHarness(t, nil)
	h.up.mu.Lock()
	h.up.refreshDead = true
	h.up.mu.Unlock()
	ctx := context.Background()
	acc := &store.Account{
		UID: "pu-bad", Name: "bad", Token: testToken("pu-bad"), RefreshToken: "rf",
		Status: store.StatusOnline, Enabled: true,
		TokenExp: time.Now().Add(time.Hour).UnixMilli(),
	}
	if err := h.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	res := h.engine.Refresher.RenewDue(ctx, 24*time.Hour, 0)
	if res.Failed != 1 || res.Renewed != 0 {
		t.Fatalf("应记为一次失败: %+v", res)
	}
	got, _ := h.st.GetByUID(ctx, "pu-bad")
	if got.Status != store.StatusRelogin {
		t.Fatalf("凭据失效应标 relogin，实际 %s", got.Status)
	}
}

// 热更新：改完参数后，下一次 runOnce 必须按新窗口执行，不需要重启。
func TestRenewerHotUpdateWindow(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	acc := &store.Account{
		UID: "pu-hot", Name: "hot", Token: testToken("pu-hot"), RefreshToken: "rf",
		Status: store.StatusOnline, Enabled: true,
		TokenExp: time.Now().Add(10 * 24 * time.Hour).UnixMilli(), // 10 天后到期
	}
	if err := h.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	rw := NewRenewer(h.engine.Refresher, RenewConfig{
		Window: 3 * 24 * time.Hour, Interval: time.Hour, Gap: 0,
	})
	// 3 天窗口够不到 10 天后的号
	res, ran := rw.RunOnce(ctx)
	if !ran || res.Checked != 0 {
		t.Fatalf("窗口外不该被续期: ran=%v %+v", ran, res)
	}
	// 把窗口放大到 15 天，同一个号应该被纳入
	rw.Update(RenewConfig{Window: 15 * 24 * time.Hour, Interval: time.Hour})
	res, ran = rw.RunOnce(ctx)
	if !ran || res.Checked != 1 || res.Renewed != 1 {
		t.Fatalf("热更新后应按新窗口续期: ran=%v %+v", ran, res)
	}
	got, _ := h.st.GetByUID(ctx, "pu-hot")
	if got.Token != "new-access-token" {
		t.Fatal("续期结果未落库")
	}
}

// 关闭后 RunOnce 不应做任何事，但重新打开能立刻生效。
func TestRenewerDisableReenable(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	acc := &store.Account{
		UID: "pu-toggle", Name: "toggle", Token: testToken("pu-toggle"), RefreshToken: "rf",
		Status: store.StatusOnline, Enabled: true,
		TokenExp: time.Now().Add(time.Hour).UnixMilli(),
	}
	if err := h.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	rw := NewRenewer(h.engine.Refresher, RenewConfig{})
	if _, ran := rw.RunOnce(ctx); ran {
		t.Fatal("关闭状态不该执行续期")
	}
	rw.Update(RenewConfig{Window: 24 * time.Hour, Interval: time.Hour})
	res, ran := rw.RunOnce(ctx)
	if !ran || res.Checked != 1 {
		t.Fatalf("重新打开后应续期: ran=%v %+v", ran, res)
	}
	if st := rw.Status(); st["enabled"] != true {
		t.Fatalf("状态应显示已启用: %+v", st)
	}
}

// 并发刷新冲突（200822）不是凭证失效：用库里最新凭据重试一次即可，
// 不能把好号打成 relogin。
func TestRefreshConflictRetriesWithLatestToken(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != raccoon.PathRefresh {
			w.WriteHeader(404)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls++
		seen = append(seen, body["refresh_token"])
		n := calls
		mu.Unlock()
		if n == 1 {
			// 第一次用的旧凭据已被并发刷新替换
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"code":200822,"message":"refresh_token 已被并发刷新替换"}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"access_token":"`+testToken("pu-conflict")+`","refresh_token":"rt-new"}}`)
	}))
	defer up.Close()

	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	acc := &store.Account{UID: "pu-conflict", Token: "old-token", RefreshToken: "rt-old",
		Status: store.StatusOnline, Enabled: true, Meta: map[string]any{}}
	if err := st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	// 库里已是并发刷新后的新凭据
	fresh := "rt-fresh"
	if err := st.UpdateAccount(ctx, acc.ID, store.AccountPatch{RefreshToken: &fresh}); err != nil {
		t.Fatal(err)
	}

	client := raccoon.NewClient(up.URL, 5*time.Second)
	r := NewRefresher(st, client, 5*time.Minute)
	tok, err := r.RefreshNow(ctx, acc)
	if err != nil {
		t.Fatalf("并发冲突应重试成功，实际 %v", err)
	}
	if tok == "" {
		t.Fatal("应拿到新 token")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "rt-old" || seen[1] != "rt-fresh" {
		t.Fatalf("应先用旧凭据、再退到库里的新凭据，实际 %v", seen)
	}
	got, _ := st.GetAccount(ctx, acc.ID)
	if got.Status == store.StatusRelogin {
		t.Fatal("并发冲突不该把账号打成 relogin")
	}
}
