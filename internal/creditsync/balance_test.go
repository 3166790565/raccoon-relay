package creditsync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// stubAuth 模拟 Refresher：不刷新，直接把账号现有 token 交出去。
type stubAuth struct{ err error }

func (s stubAuth) EnsureFresh(_ context.Context, acc *store.Account) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return acc.Token, nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newAccount(t *testing.T, st *store.Store, credits int64) *store.Account {
	t.Helper()
	a := &store.Account{
		UID: "pu-1", Token: "tok", Status: store.StatusOnline,
		Credits: credits, Enabled: true, Meta: map[string]any{},
	}
	if err := st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// balanceServer 模拟 setting_info——余额现在的来源，不是已下线的 points/v1/balance。
func balanceServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != raccoon.PathSetting {
			// 旧端点也拒掉：走到旧端点说明 setting_info 已经失败，测试不该被它救回来
			http.NotFound(w, r)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRefreshWritesCreditsAndTimestamp(t *testing.T) {
	st := newStore(t)
	acc := newAccount(t, st, -1)
	srv := balanceServer(t, `{"code":0,"data":{"available_points":4321}}`, 0)

	b := &Balancer{ST: st, Client: raccoon.NewClient(srv.URL, 5*time.Second), Auth: stubAuth{}}
	credits, err := b.Refresh(context.Background(), acc)
	if err != nil {
		t.Fatal(err)
	}
	if credits != 4321 {
		t.Fatalf("返回余额不对: %d", credits)
	}

	got, _ := st.GetAccount(context.Background(), acc.ID)
	if got.Credits != 4321 {
		t.Fatalf("余额未写库: %d", got.Credits)
	}
	if got.CreditsAt == 0 {
		t.Fatal("credits_at 未更新，无法判断余额新鲜度")
	}
}

// 查询失败时绝不能把旧余额覆盖成 0——那会让号在面板上像是「没钱了」。
func TestRefreshKeepsOldValueOnFailure(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"401 凭证失效", `{"code":200003,"message":"authorization_verify_error"}`, 401},
		{"500 上游异常", `{"code":500,"message":"boom"}`, 500},
		{"业务码错误", `{"code":99999,"message":"whatever"}`, 200},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore(t)
			acc := newAccount(t, st, 777)
			srv := balanceServer(t, c.body, c.status)

			b := &Balancer{ST: st, Client: raccoon.NewClient(srv.URL, 5*time.Second), Auth: stubAuth{}}
			if _, err := b.Refresh(context.Background(), acc); err == nil {
				t.Fatal("应返回错误")
			}

			got, _ := st.GetAccount(context.Background(), acc.ID)
			if got.Credits != 777 {
				t.Fatalf("失败时不应改动旧余额，实际变成 %d", got.Credits)
			}
		})
	}
}

func TestRefreshPropagatesAuthError(t *testing.T) {
	st := newStore(t)
	acc := newAccount(t, st, 100)
	b := &Balancer{ST: st, Client: raccoon.NewClient("http://127.0.0.1:1", time.Second),
		Auth: stubAuth{err: errors.New("refresh_token 已被轮换")}}

	if _, err := b.Refresh(context.Background(), acc); err == nil {
		t.Fatal("刷新失败应向上返回")
	}
	got, _ := st.GetAccount(context.Background(), acc.ID)
	if got.Credits != 100 {
		t.Fatalf("刷新失败不应改动余额，实际 %d", got.Credits)
	}
}

func TestRefreshRejectsNilAccount(t *testing.T) {
	b := &Balancer{ST: newStore(t), Client: nil, Auth: stubAuth{}}
	if _, err := b.Refresh(context.Background(), nil); err == nil {
		t.Fatal("nil 账号应报错而不是 panic")
	}
}

// 连续两次刷新，后一次应覆盖前一次。
func TestRefreshOverwritesPreviousValue(t *testing.T) {
	st := newStore(t)
	acc := newAccount(t, st, 100)
	ctx := context.Background()

	srv1 := balanceServer(t, `{"code":0,"data":{"available_points":500}}`, 0)
	b1 := &Balancer{ST: st, Client: raccoon.NewClient(srv1.URL, 5*time.Second), Auth: stubAuth{}}
	if _, err := b1.Refresh(ctx, acc); err != nil {
		t.Fatal(err)
	}

	srv2 := balanceServer(t, `{"code":0,"data":{"available_points":1500}}`, 0)
	b2 := &Balancer{ST: st, Client: raccoon.NewClient(srv2.URL, 5*time.Second), Auth: stubAuth{}}
	if _, err := b2.Refresh(ctx, acc); err != nil {
		t.Fatal(err)
	}

	got, _ := st.GetAccount(ctx, acc.ID)
	if got.Credits != 1500 {
		t.Fatalf("应更新为最新余额 1500，实际 %d", got.Credits)
	}
}

// 余额巡检：过旧的号被刷新，新鲜的跳过；日日新看 SNPoolAt，
// 小浣熊/外部渠道看 CreditsAt；停用号与「需重登」号不参与。
func TestRefreshStaleOnlyTouchesStale(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	old := time.Now().Add(-2 * time.Hour).UnixMilli()
	now := time.Now().UnixMilli()

	snStale := &store.Account{UID: "sn-old", Provider: store.ProviderSenseNova, Token: "k",
		Status: store.StatusOnline, Credits: 0, Enabled: true, SNPoolAt: 0, SNToken: "t"}
	snFresh := &store.Account{UID: "sn-new", Provider: store.ProviderSenseNova, Token: "k",
		Status: store.StatusOnline, Credits: 100, Enabled: true, SNPoolAt: now, SNToken: "t"}
	rcStale := &store.Account{UID: "rc-old", Token: "tok", Status: store.StatusOnline,
		Credits: 7, CreditsAt: old, Enabled: true}
	rcFresh := &store.Account{UID: "rc-new", Token: "tok", Status: store.StatusOnline,
		Credits: 9, CreditsAt: now, Enabled: true}
	disabled := &store.Account{UID: "rc-off", Token: "tok", Status: store.StatusOnline,
		Credits: 5, CreditsAt: old, Enabled: false}
	relogin := &store.Account{UID: "rc-relogin", Token: "tok", Status: store.StatusRelogin,
		Credits: 5, CreditsAt: old, Enabled: true}
	for _, a := range []*store.Account{snStale, snFresh, rcStale, rcFresh, disabled, relogin} {
		if err := st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	srv := balanceServer(t, `{"code":0,"data":{"available_points":4321}}`, 0)
	b := &Balancer{
		ST: st, Client: raccoon.NewClient(srv.URL, 5*time.Second), Auth: stubAuth{},
		SN: fakeSNBalance{remaining: 8765},
	}
	n := b.RefreshStale(ctx, 30*time.Minute)
	if n != 2 {
		t.Fatalf("应刷新 2 个过旧账号（1 日日新 + 1 小浣熊），实际 %d", n)
	}
	if got, _ := st.GetAccount(ctx, snStale.ID); got.Credits != 8765 {
		t.Fatalf("过旧日日新应刷新到 8765，实际 %d", got.Credits)
	}
	if got, _ := st.GetAccount(ctx, rcStale.ID); got.Credits != 4321 {
		t.Fatalf("过旧小浣熊应刷新到 4321，实际 %d", got.Credits)
	}
	if got, _ := st.GetAccount(ctx, snFresh.ID); got.Credits != 100 {
		t.Fatalf("新鲜日日新不应被刷新，实际 %d", got.Credits)
	}
	if got, _ := st.GetAccount(ctx, rcFresh.ID); got.Credits != 9 {
		t.Fatalf("新鲜小浣熊不应被刷新，实际 %d", got.Credits)
	}
	for _, a := range []*store.Account{disabled, relogin} {
		if got, _ := st.GetAccount(ctx, a.ID); got.Credits != 5 {
			t.Fatalf("%s 不应参与巡检，实际 %d", a.UID, got.Credits)
		}
	}
}

// fakeSNBalance 是 SNBalance 的桩。
type fakeSNBalance struct{ remaining float64 }

func (f fakeSNBalance) FetchUsageFor(context.Context, *store.Account) (float64, string, error) {
	return f.remaining, `{"pools":[]}`, nil
}
