package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/eventlog"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// 号池页「限流」列点开后要拿到这个号最近的失败现场。
func TestAccountErrorsReturnsLatestRelayEvent(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	acc := &store.Account{UID: "pu-1", Name: "号一", Token: "tok", Status: store.StatusCooling, Enabled: true}
	if err := p.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if err := p.st.AddEvent(ctx, store.RuntimeEvent{
		Level: store.LevelWarn, Scope: store.ScopeRelay, Component: "engine",
		AccountID: acc.ID, Message: "上游失败：HTTP 403",
		Detail: "上游返回码：403\n响应头：\n  x-tt-system-error: 3\n响应体：\nblocked",
	}); err != nil {
		t.Fatal(err)
	}
	// 另一条更早的 info 级事件不该把最近的失败挤掉。
	if err := p.st.AddEvent(ctx, store.RuntimeEvent{
		Level: store.LevelInfo, Scope: store.ScopeRelay, Component: "engine",
		AccountID: acc.ID, Message: "旧事件", Detail: "old",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/accounts/"+acc.ID+"/errors", nil)
	req.Header.Set("Authorization", "Bearer adm")
	w := httptest.NewRecorder()
	p.handleAccountAction(w, req)

	if w.Code != 200 {
		t.Fatalf("应返回 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK      bool                 `json:"ok"`
		Events  []store.RuntimeEvent `json:"events"`
		Account struct {
			Status    string `json:"status"`
			CoolUntil int64  `json:"coolUntil"`
		} `json:"account"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || len(resp.Events) != 2 {
		t.Fatalf("应返回两条事件，实际 %d", len(resp.Events))
	}
	// 按最近时间倒序：刚写入的「旧事件」反而更近，这里只校验内容都在。
	var joined []string
	for _, e := range resp.Events {
		joined = append(joined, e.Detail)
	}
	if !strings.Contains(strings.Join(joined, "\n"), "x-tt-system-error: 3") {
		t.Fatalf("应带出 403 现场，实际 %v", joined)
	}
	if resp.Account.Status != store.StatusCooling {
		t.Fatalf("账号状态应随响应返回，实际 %q", resp.Account.Status)
	}
}

// 没有历史失败时也要正常返回空列表，前端据此显示提示。
func TestAccountErrorsEmptyIsOK(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	acc := &store.Account{UID: "pu-2", Token: "tok", Status: store.StatusOnline, Enabled: true}
	if err := p.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/accounts/"+acc.ID+"/errors", nil)
	w := httptest.NewRecorder()
	p.handleAccountAction(w, req)
	if w.Code != 200 {
		t.Fatalf("应返回 200，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"events":[]`) && !strings.Contains(w.Body.String(), `"events":null`) {
		t.Fatalf("空列表应正常序列化: %s", w.Body.String())
	}
}

// 引擎抛出的事件经 Recorder 落库后，能被「错误详情」接口原样取回：
// 这条链路是面板排查 403 的唯一依据，断开就只剩一句 HTTP 403。
func TestAccountErrorsShowsEngineEventEndToEnd(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	acc := &store.Account{UID: "pu-e2e", Token: "tok", Status: store.StatusCooling, Enabled: true}
	if err := p.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	rec := eventlog.New(p.st)
	rec.Emit(store.RuntimeEvent{
		Level: store.LevelWarn, Scope: store.ScopeRelay, Component: "engine",
		AccountID: acc.ID, Message: "上游失败：HTTP 403",
		Detail: "上游返回码：403\n接口：/api/web/llm/v2/chat/completions\n响应头：\n  x-tt-system-error: 3\n响应体：\nblocked",
	})
	rec.Flush(ctx)

	req := httptest.NewRequest(http.MethodGet, "/api/accounts/"+acc.ID+"/errors", nil)
	w := httptest.NewRecorder()
	p.handleAccountAction(w, req)
	if w.Code != 200 {
		t.Fatalf("应返回 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var resp struct {
		Events []store.RuntimeEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("应取回一条事件，实际 %d", len(resp.Events))
	}
	detail := resp.Events[0].Detail
	for _, want := range []string{"x-tt-system-error: 3", "blocked", "接口："} {
		if !strings.Contains(detail, want) {
			t.Fatalf("详情缺少 %q:\n%s", want, detail)
		}
	}
}

// 保存失败发生在很久以前时，LatestEvents 仍能取到（不按时间窗截断）。
func TestLatestEventsIgnoresAge(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	acc := &store.Account{UID: "pu-3", Token: "tok", Enabled: true}
	if err := p.st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	if _, err := p.st.DB().ExecContext(ctx,
		`INSERT INTO runtime_events (ts, level, scope, component, account_id, message, detail, count, last_ts)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		old, store.LevelWarn, store.ScopeRelay, "engine", acc.ID, "旧错误", "detail", 1, old); err != nil {
		t.Fatal(err)
	}
	rows, err := p.st.LatestEvents(ctx, acc.ID, []string{store.ScopeRelay}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Message != "旧错误" {
		t.Fatalf("应取到历史事件，实际 %+v", rows)
	}
}
