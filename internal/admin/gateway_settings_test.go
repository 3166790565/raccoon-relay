package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/modelmeta"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/sanitize"
)

// gatewayPanel 构造一个带 relay 快照的最小面板，并在每个用例后清空全局
// 出站策略（它是进程级单例）。
func gatewayPanel(t *testing.T) (*Panel, context.Context) {
	t.Helper()
	p, _ := newConcurrencyPanel(t)
	t.Cleanup(func() { relay.SetGatewayPolicy(relay.GatewayPolicy{}) })
	return p, context.Background()
}

// 保存后立即热更新到 relay 快照，并落库。
func TestPutGatewaySettingsHotUpdatesAndPersists(t *testing.T) {
	p, ctx := gatewayPanel(t)
	w := putSettings(t, p, `{"sanitizeEnabled":false,"developerRoleCompat":false,"modelsDevEnabled":false}`)
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	policy := relay.CurrentGatewayPolicy()
	if policy.SanitizeOn || policy.DeveloperRoleCompat || policy.ModelsDev {
		t.Fatalf("热更新未生效: %+v", policy)
	}
	if raw := p.st.GetSetting(ctx, keySanitizeEnabled, ""); raw != "false" {
		t.Fatalf("sanitizeEnabled 未持久化，实际 %q", raw)
	}
	if raw := p.st.GetSetting(ctx, keyDeveloperRoleCompat, ""); raw != "false" {
		t.Fatalf("developerRoleCompat 未持久化，实际 %q", raw)
	}
	if raw := p.st.GetSetting(ctx, keyModelsDevEnabled, ""); raw != "false" {
		t.Fatalf("modelsDevEnabled 未持久化，实际 %q", raw)
	}
	// GET 要返回当前生效值。
	got := getSettings(t, p)
	if got["sanitizeEnabled"] != false || got["modelsDevEnabled"] != false {
		t.Fatalf("GET 未返回生效值: %v", got)
	}
}

// 词表保存后要编译进 relay 快照，且能查回；[] = 恢复内置。
func TestPutSanitizeRulesCompilesAndRestores(t *testing.T) {
	p, ctx := gatewayPanel(t)
	body := `{"sanitizeEnabled":true,"sanitizeRules":[{"id":"r1","name":"One","enabled":true,` +
		`"matchType":"literal","pattern":"ACME","replacement":"acme"}]}`
	if w := putSettings(t, p, body); w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	policy := relay.CurrentGatewayPolicy()
	if policy.SanitizeRules == nil {
		t.Fatal("词表未下发到 relay")
	}
	entries := policy.SanitizeRules.Entries()
	if len(entries) != 1 || entries[0].Pattern != "ACME" {
		t.Fatalf("词表不对: %+v", entries)
	}
	if raw := p.st.GetSetting(ctx, keySanitizeRules, ""); !strings.Contains(raw, "ACME") {
		t.Fatalf("词表未持久化: %q", raw)
	}

	// 空数组 = 恢复内置。
	if w := putSettings(t, p, `{"sanitizeRules":[]}`); w.Code != 200 {
		t.Fatalf("恢复内置失败 %d: %s", w.Code, w.Body.String())
	}
	policy = relay.CurrentGatewayPolicy()
	if len(policy.SanitizeRules.Entries()) != len(sanitize.DefaultRuleSet().Entries()) {
		t.Fatalf("应恢复内置词表，实际 %d 条", len(policy.SanitizeRules.Entries()))
	}
}

// 非法的词表整体拒绝，且不改动已生效的配置。
func TestPutSanitizeRulesRejectsInvalid(t *testing.T) {
	p, _ := gatewayPanel(t)
	_ = putSettings(t, p, `{"sanitizeEnabled":true,"sanitizeRules":[{"id":"keep","name":"Keep",`+
		`"enabled":true,"matchType":"literal","pattern":"A","replacement":"B"}]}`)
	if w := putSettings(t, p, `{"sanitizeRules":[{"id":"","name":"Bad","enabled":true,`+
		`"matchType":"regex","pattern":"("}]}`); w.Code != 400 {
		t.Fatalf("坏正则应被拒绝，实际 %d", w.Code)
	}
	if w := putSettings(t, p, `{"sanitizeRules":[{"id":"bad","name":"","enabled":true,`+
		`"matchType":"literal","pattern":"x"}]}`); w.Code != 400 {
		t.Fatalf("空名称应被拒绝，实际 %d", w.Code)
	}
	// 现有词表保持不变。
	policy := relay.CurrentGatewayPolicy()
	entries := policy.SanitizeRules.Entries()
	if len(entries) != 1 || entries[0].Pattern != "A" {
		t.Fatalf("非法保存不应改动现有词表: %+v", entries)
	}
}

// 重启后从库里恢复。
func TestApplyStoredGatewaySettings(t *testing.T) {
	p, ctx := gatewayPanel(t)
	_ = p.st.SetSetting(ctx, keySanitizeEnabled, "false")
	_ = p.st.SetSetting(ctx, keyDeveloperRoleCompat, "true")
	_ = p.st.SetSetting(ctx, keyModelsDevEnabled, "false")
	_ = p.st.SetSetting(ctx, keySanitizeProviders, `["sensenova"]`)
	_ = p.st.SetSetting(ctx, keySanitizeRules,
		`[{"id":"saved","name":"Saved","enabled":true,"matchType":"literal","pattern":"X","replacement":"Y"}]`)
	// 先写入一份「网络刷新」快照，验证重启后优先用它而不是内置快照。
	cache := `{"generatedAt":999,"source":"network","models":{"cached":{"canonical":"x/cached","context":42}}}`
	_ = p.st.SetSetting(ctx, keyModelsDevCache, cache)

	p.gatewayState = nil // 模拟重启：进程内状态清零
	p.applyStoredGatewaySettings(ctx)

	policy := relay.CurrentGatewayPolicy()
	if policy.SanitizeOn || !policy.DeveloperRoleCompat || policy.ModelsDev {
		t.Fatalf("持久化值未恢复: %+v", policy)
	}
	if len(policy.SanitizeProviders) != 1 || policy.SanitizeProviders[0] != "sensenova" {
		t.Fatalf("提供商范围未恢复: %+v", policy.SanitizeProviders)
	}
	if entries := policy.SanitizeRules.Entries(); len(entries) != 1 || entries[0].ID != "saved" {
		t.Fatalf("自定义词表未恢复: %+v", entries)
	}
	status := modelmeta.CurrentStatus()
	if status.Source != "cache" || status.Count != 1 {
		t.Fatalf("应优先装载库里的刷新结果，实际 %+v", status)
	}
	if _, ok := modelmeta.Lookup("cached"); !ok {
		t.Fatal("刷新结果里的模型查不到")
	}
}

// 刷新链路：拉取成功要落库、换内存、推快照；拉取失败保留旧值。
func TestRefreshModelsDev(t *testing.T) {
	p, ctx := gatewayPanel(t)
	_ = putSettings(t, p, `{"modelsDevEnabled":true}`)
	p.SetMetaFetch(func(ctx context.Context, url string) ([]byte, error) {
		if strings.Contains(url, "api.json") {
			return []byte(`{"zhipuai":{"models":{"glm-5.3":{"id":"glm-5.3","canonical_model_id":"zhipuai/glm-5.3","reasoning_options":[{"type":"effort","values":["low","high"]}]}}}}`), nil
		}
		return []byte(`{"zhipuai/glm-5.3":{"type":"chat","reasoning":true,"tool_call":true,"modalities":{"input":["text"],"output":["text"]},"limit":{"context":500,"output":100}}}`), nil
	})
	ok, msg, count := p.refreshModelsDev(ctx)
	if !ok || count != 1 {
		t.Fatalf("刷新应成功: ok=%v msg=%s count=%d", ok, msg, count)
	}
	if raw := p.st.GetSetting(ctx, keyModelsDevCache, ""); raw == "" {
		t.Fatal("刷新结果未落库")
	}
	if _, ok := modelmeta.Lookup("glm-5.3"); !ok {
		t.Fatal("刷新后查询不到模型")
	}
	if rec, _ := modelmeta.Lookup("glm-5.3"); len(rec.Levels) != 2 {
		t.Fatalf("档位未进入快照: %+v", rec)
	}

	// 失败不该清空现有索引。
	p.SetMetaFetch(func(ctx context.Context, url string) ([]byte, error) {
		return nil, errorText("network down")
	})
	ok, _, _ = p.refreshModelsDev(ctx)
	if ok {
		t.Fatal("拉取失败应返回 ok=false")
	}
	if _, ok := modelmeta.Lookup("glm-5.3"); !ok {
		t.Fatal("失败不应清空现有索引")
	}

	// 关闭后刷新直接跳过。
	_ = putSettings(t, p, `{"modelsDevEnabled":false}`)
	ok, msg, _ = p.refreshModelsDev(ctx)
	if ok || !strings.Contains(msg, "关闭") {
		t.Fatalf("关闭后应跳过: ok=%v msg=%s", ok, msg)
	}
}

// REST 端点与 /api/settings 共用状态。
func TestGatewayRESTHandlers(t *testing.T) {
	p, _ := gatewayPanel(t)
	// PUT /api/gateway/sanitize
	req := newAdminRequest(http.MethodPut, "/api/gateway/sanitize",
		`{"enabled":false,"providers":["raccoon"]}`)
	w := httptest.NewRecorder()
	p.handlePutSanitize(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT sanitize 失败 %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["enabled"] != false {
		t.Fatalf("响应未回显新状态: %v", resp)
	}
	if policy := relay.CurrentGatewayPolicy(); policy.SanitizeOn || len(policy.SanitizeProviders) != 1 {
		t.Fatalf("快照未更新: %+v", policy)
	}

	// GET /api/gateway/modelsdev
	req = newAdminRequest(http.MethodGet, "/api/gateway/modelsdev", "")
	w = httptest.NewRecorder()
	p.handleGetModelsDev(w, req)
	if w.Code != 200 {
		t.Fatalf("GET modelsdev 失败 %d", w.Code)
	}
}

// developer 角色在出站前被归一为 system（relay 层的行为）。
func TestNormalizeDeveloperRole(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "developer", "content": "be brief"},
			map[string]any{"role": "Developer", "content": "case variant"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	next, changed := relay.NormalizeDeveloperRoleForTest(body)
	if !changed {
		t.Fatal("应识别到 developer 消息")
	}
	msgs := next["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "system" {
		t.Fatalf("角色未归一: %+v", msgs)
	}
	if msgs[2].(map[string]any)["role"] != "user" {
		t.Fatal("其它角色不应被改动")
	}
	// 原始体不受影响。
	if body["messages"].([]any)[0].(map[string]any)["role"] != "developer" {
		t.Fatal("原请求体被意外改写")
	}
	// 没有 developer 消息时零拷贝。
	if _, changed := relay.NormalizeDeveloperRoleForTest(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}); changed {
		t.Fatal("无 developer 消息不应改写")
	}
}

// newAdminRequest 构造一个已带管理口令的请求。
func newAdminRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	return req
}

type errorText string

func (e errorText) Error() string { return string(e) }
