package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/store"
)

func newAccountPanel(t *testing.T) (*Panel, context.Context) {
	t.Helper()
	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{AdminToken: "adm", DataDir: t.TempDir(), MaxRetries: 3, PoolStrategy: "load_balance"}
	p := &Panel{
		cfg:    cfg,
		st:     st,
		engine: relay.BuildEngine(cfg, st, nil, nil, nil, nil),
		po:     pool.New(st, "load_balance", pool.NewCooldownBook(st, true)),
		cool:   pool.NewCooldownBook(st, true),
	}
	return p, context.Background()
}

func addAcc(t *testing.T, p *Panel, ctx context.Context, uid string) *store.Account {
	t.Helper()
	a := &store.Account{UID: uid, Name: uid, Token: "tok", Status: store.StatusOnline, Enabled: true}
	if err := p.st.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func patchAccount(t *testing.T, p *Panel, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/accounts/"+id, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer adm")
	w := httptest.NewRecorder()
	p.handleAccountAction(w, req)
	return w
}

func postBatch(t *testing.T, p *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/batch", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer adm")
	w := httptest.NewRecorder()
	p.handleBatchAccounts(w, req)
	return w
}

// 单个账号设置并发上限，落库并可通过列表接口回读。
func TestPatchAccountMaxConcurrent(t *testing.T) {
	p, ctx := newAccountPanel(t)
	a := addAcc(t, p, ctx, "u1")

	if w := patchAccount(t, p, a.ID, `{"maxConcurrent":3}`); w.Code != 200 {
		t.Fatalf("设置失败 %d: %s", w.Code, w.Body.String())
	}
	got, err := p.st.GetAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxConcurrent != 3 {
		t.Fatalf("应落库为 3，实际 %d", got.MaxConcurrent)
	}

	// 列表接口要把这个字段带出去，前端才能展示/回填。
	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	w := httptest.NewRecorder()
	p.handleListAccounts(w, req)
	var resp struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Accounts) != 1 || resp.Accounts[0]["maxConcurrent"] != float64(3) {
		t.Fatalf("列表应返回 maxConcurrent=3，实际 %v", resp.Accounts)
	}

	// 负数应拒绝。
	if w := patchAccount(t, p, a.ID, `{"maxConcurrent":-1}`); w.Code != 400 {
		t.Fatalf("负数应拒绝，实际 %d", w.Code)
	}
	// 0 合法，表示不单独限制。
	if w := patchAccount(t, p, a.ID, `{"maxConcurrent":0}`); w.Code != 200 {
		t.Fatalf("0 应接受，实际 %d", w.Code)
	}
}

// 批量设置只影响选中的账号。
func TestBatchSetMaxConcurrentSelected(t *testing.T) {
	p, ctx := newAccountPanel(t)
	a1 := addAcc(t, p, ctx, "u1")
	a2 := addAcc(t, p, ctx, "u2")
	a3 := addAcc(t, p, ctx, "u3")

	w := postBatch(t, p, `{"action":"setMaxConcurrent","ids":["`+a1.ID+`","`+a2.ID+`"],"value":2}`)
	if w.Code != 200 {
		t.Fatalf("批量设置失败 %d: %s", w.Code, w.Body.String())
	}
	check := func(id string, want int64) {
		t.Helper()
		got, _ := p.st.GetAccount(ctx, id)
		if got.MaxConcurrent != want {
			t.Fatalf("账号 %s 应为 %d，实际 %d", id, want, got.MaxConcurrent)
		}
	}
	check(a1.ID, 2)
	check(a2.ID, 2)
	check(a3.ID, 0)
}

// ids 为空表示全部账号。
func TestBatchSetMaxConcurrentAll(t *testing.T) {
	p, ctx := newAccountPanel(t)
	a1 := addAcc(t, p, ctx, "u1")
	a2 := addAcc(t, p, ctx, "u2")

	w := postBatch(t, p, `{"action":"setMaxConcurrent","ids":[],"value":5}`)
	if w.Code != 200 {
		t.Fatalf("全部设置失败 %d: %s", w.Code, w.Body.String())
	}
	for _, id := range []string{a1.ID, a2.ID} {
		got, _ := p.st.GetAccount(ctx, id)
		if got.MaxConcurrent != 5 {
			t.Fatalf("账号 %s 应为 5，实际 %d", id, got.MaxConcurrent)
		}
	}
}

// 非法值整体拒绝。
func TestBatchSetMaxConcurrentRejectsInvalid(t *testing.T) {
	p, ctx := newAccountPanel(t)
	a := addAcc(t, p, ctx, "u1")

	if w := postBatch(t, p, `{"action":"setMaxConcurrent","ids":[],"value":-2}`); w.Code != 400 {
		t.Fatalf("负数应拒绝，实际 %d", w.Code)
	}
	if w := postBatch(t, p, `{"action":"setMaxConcurrent","ids":[]}`); w.Code != 400 {
		t.Fatalf("缺 value 应拒绝，实际 %d", w.Code)
	}
	got, _ := p.st.GetAccount(ctx, a.ID)
	if got.MaxConcurrent != 0 {
		t.Fatalf("拒绝后不应改动，实际 %d", got.MaxConcurrent)
	}
}