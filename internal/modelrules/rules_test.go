package modelrules

import (
	"context"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/store"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(t.TempDir(), "test-secret-key-0123456789abcdef", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewManager(context.Background(), st)
}

func strPtr(s string) *string  { return &s }
func testBoolPtr(b bool) *bool { return &b }

func TestMappingResolvePrefersExplicitAlias(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	// 自映射 + 同名的显式别名：显式别名优先。
	if err := m.AddMapping(ctx, "sensenova", "fast", "fast", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMapping(ctx, "sensenova", "fast", "deepseek-flash", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	target, _, ok := m.Rules.Resolve("fast")
	if !ok || target != "deepseek-flash" {
		t.Fatalf("应命中显式别名，实际 %q ok=%v", target, ok)
	}
}

func TestMappingDisabledAliasNotResolved(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	if err := m.AddMapping(ctx, "sensenova", "alias", "glm-5.3", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMapping(ctx, "sensenova", "alias", "glm-5.3", nil, testBoolPtr(false), false); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := m.Rules.Resolve("alias"); ok {
		t.Fatal("关闭的映射不应参与解析")
	}
}

func TestMappingReasoningTriState(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	// reasoning 没带 = 不动已有等级。
	if err := m.AddMapping(ctx, "sensenova", "a", "kimi-k3", strPtr("high"), nil, true); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMapping(ctx, "sensenova", "a", "kimi-k3", nil, testBoolPtr(false), false); err != nil {
		t.Fatal(err)
	}
	mp, _ := m.Rules.MappingFor("sensenova", "a", "kimi-k3")
	if mp.Reasoning != "high" {
		t.Fatalf("没带 reasoning 时不应清空，实际 %q", mp.Reasoning)
	}
	// 空串 = 显式清空。
	if err := m.AddMapping(ctx, "sensenova", "a", "kimi-k3", strPtr(""), nil, false); err != nil {
		t.Fatal(err)
	}
	mp, _ = m.Rules.MappingFor("sensenova", "a", "kimi-k3")
	if mp.Reasoning != "" {
		t.Fatalf("空串应清空等级，实际 %q", mp.Reasoning)
	}
}

func TestDefaultMappingCannotBeRemoved(t *testing.T) {
	m := newManager(t)
	err := m.RemoveMapping(context.Background(), "sensenova", "glm-5.3", "glm-5.3")
	if err == nil {
		t.Fatal("自映射不应允许删除")
	}
	if e := AsError(err); e.Status != 400 {
		t.Fatalf("应为 400，实际 %d", e.Status)
	}
}

func TestCapabilitiesValidationAndClear(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	if err := m.SetCapabilities(ctx, "sensenova", "m", map[string]any{
		"maxInputTokens": float64(200000),
		"supportsImages": true,
	}); err != nil {
		t.Fatal(err)
	}
	got := m.Rules.CapabilityOverrides("sensenova", "m")
	if got["supportsImages"] != true {
		t.Fatalf("覆盖未生效: %+v", got)
	}
	// 非法值整体拒绝，且不改动已有覆盖。
	if err := m.SetCapabilities(ctx, "sensenova", "m", map[string]any{
		"maxInputTokens": float64(-1),
	}); err == nil {
		t.Fatal("负数应被拒绝")
	}
	if got := m.Rules.CapabilityOverrides("sensenova", "m"); got["supportsImages"] != true {
		t.Fatal("非法写入不应破坏已有覆盖")
	}
	// 未知键拒绝。
	if err := m.SetCapabilities(ctx, "sensenova", "m", map[string]any{"bogus": true}); err == nil {
		t.Fatal("未知键应被拒绝")
	}
	// nil 清空。
	if err := m.SetCapabilities(ctx, "sensenova", "m", nil); err != nil {
		t.Fatal(err)
	}
	if got := m.Rules.CapabilityOverrides("sensenova", "m"); len(got) != 0 {
		t.Fatalf("应已清空，实际 %+v", got)
	}
}

func TestStateAndPersistenceAcrossRestart(t *testing.T) {
	st, err := store.Open(t.TempDir(), "test-secret-key-0123456789abcdef", true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	m := NewManager(ctx, st)
	if err := m.SetState(ctx, "sensenova", "glm-5.3", false); err != nil {
		t.Fatal(err)
	}
	if err := m.AddCustom(ctx, "sensenova", "future-model"); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：同一份数据库重新加载。
	m2 := NewManager(ctx, st)
	if !m2.Rules.Hidden("glm-5.3") {
		t.Fatal("停用状态未持久化")
	}
	if got := m2.Rules.CustomModels("sensenova"); len(got) != 1 || got[0] != "future-model" {
		t.Fatalf("自定义模型未持久化: %+v", got)
	}
	// 重新启用要能从停用列表里移除。
	if err := m2.SetState(ctx, "sensenova", "glm-5.3", true); err != nil {
		t.Fatal(err)
	}
	if m2.Rules.Hidden("glm-5.3") {
		t.Fatal("重新启用后不应再隐藏")
	}
}

func TestBuildViewMarksDanglingMappings(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	cats := []ProviderCatalog{{
		Provider: "sensenova", Label: "日日新", Source: "remote",
		Models: []CatalogModel{
			{ID: "glm-5.3", ContextLength: 1048576, MaxOutputTokens: 131072, SupportsTools: true},
		},
	}}
	// 指向一个不存在的上游模型。
	if err := m.AddMapping(ctx, "sensenova", "ghost", "removed-model", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	// 指向存在的模型并启停它。
	if err := m.AddMapping(ctx, "sensenova", "fast", "glm-5.3", strPtr("high"), nil, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetState(ctx, "sensenova", "glm-5.3", false); err != nil {
		t.Fatal(err)
	}

	view := BuildView(m.Rules, cats)
	if len(view.Models) != 1 {
		t.Fatalf("应有 1 行模型，实际 %d", len(view.Models))
	}
	row := view.Models[0]
	if row.Enabled {
		t.Fatal("模型应显示为停用")
	}
	if len(row.Aliases) != 1 || row.Aliases[0] != "fast" {
		t.Fatalf("别名不对: %+v", row.Aliases)
	}
	if row.Capabilities.MaxInputTokens == nil || *row.Capabilities.MaxInputTokens != 1048576 {
		t.Fatalf("上下文能力未透出: %+v", row.Capabilities)
	}

	var ghost, fast *MappingRow
	for i := range view.Mappings {
		switch view.Mappings[i].Alias {
		case "ghost":
			ghost = &view.Mappings[i]
		case "fast":
			fast = &view.Mappings[i]
		}
	}
	if ghost == nil || !ghost.Dangling {
		t.Fatalf("ghost 应标记为未挂载: %+v", ghost)
	}
	if fast == nil || fast.Dangling {
		t.Fatalf("fast 应挂载在模型行上: %+v", fast)
	}
	if fast.Reasoning != "high" {
		t.Fatalf("思考等级未透出: %+v", fast)
	}
}

func TestBuildViewIncludesCustomModels(t *testing.T) {
	m := newManager(t)
	ctx := context.Background()
	if err := m.AddCustom(ctx, "sensenova", "brand-new-model"); err != nil {
		t.Fatal(err)
	}
	view := BuildView(m.Rules, []ProviderCatalog{{
		Provider: "sensenova", Label: "日日新", Source: "remote",
		Models: []CatalogModel{{ID: "glm-5.3"}},
	}})
	var found *ModelRow
	for i := range view.Models {
		if view.Models[i].ID == "brand-new-model" {
			found = &view.Models[i]
		}
	}
	if found == nil || !found.IsCustom {
		t.Fatalf("自定义模型应出现在视图里: %+v", view.Models)
	}
}
