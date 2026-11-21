package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/sensenova"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// 号池列表必须把两种供应商分开：日日新账号带 provider 与额度窗口，
// 小浣熊账号仍走原字段，前端据此分组。
func TestListAccountsSplitsProviders(t *testing.T) {
	p, ctx := newAccountPanel(t)
	raccoon := &store.Account{UID: "pu-1", Name: "小浣熊号", Token: "tok", Status: store.StatusOnline, Enabled: true}
	if err := p.st.CreateAccount(ctx, raccoon); err != nil {
		t.Fatal(err)
	}
	usage := &sensenova.Usage{Plan: "Free Plan", Pools: []sensenova.Pool{{
		Name: "通用积分池", Type: "default",
		Window5h: &sensenova.PoolWindow{Limit: 60000, Remaining: 54000},
	}}}
	poolJSON := usage.MarshalStored()
	sn := &store.Account{
		UID: "tenant-9", Name: "日日新号", Provider: store.ProviderSenseNova,
		Status: store.StatusOnline, Enabled: true,
		SNUsername: "u@example.com", SNToken: "tok", SNPool: poolJSON,
	}
	if err := p.st.CreateAccount(ctx, sn); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	req.Header.Set("Authorization", "Bearer adm")
	w := httptest.NewRecorder()
	p.handleListAccounts(w, req)
	if w.Code != 200 {
		t.Fatalf("HTTP %d：%s", w.Code, w.Body.String())
	}
	var resp struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var raccoonRow, snRow map[string]any
	for _, a := range resp.Accounts {
		switch a["provider"] {
		case "raccoon":
			raccoonRow = a
		case "sensenova":
			snRow = a
		}
	}
	if raccoonRow == nil || snRow == nil {
		t.Fatalf("应各有一行，实际 %d 行", len(resp.Accounts))
	}
	if raccoonRow["providerName"] != "小浣熊" {
		t.Fatalf("小浣熊 providerName=%v", raccoonRow["providerName"])
	}
	if snRow["providerName"] != "日日新" {
		t.Fatalf("日日新 providerName=%v", snRow["providerName"])
	}
	wins, ok := snRow["snWindows"].([]any)
	if !ok || len(wins) != 1 {
		t.Fatalf("日日新应有一格额度窗口，实际 %v", snRow["snWindows"])
	}
	win := wins[0].(map[string]any)
	if win["percent"].(float64) != 90 {
		t.Fatalf("5h 窗口剩余百分比=%v", win["percent"])
	}
	if _, has := raccoonRow["snWindows"]; has {
		t.Fatal("小浣熊行不应带日日新字段")
	}
}
