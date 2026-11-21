package modelrules

import (
	"sort"
	"strings"
)

// CatalogModel 是中立的模型条目形状。由装配层从日日新实时清单 / 内置快照
// 映射进来，避免本包依赖具体上游包。
type CatalogModel struct {
	ID              string
	Name            string
	ContextLength   int
	MaxOutputTokens int
	SupportsImages  bool
	SupportsTools   bool
	SupportsReason  bool
	SupportsVideo   bool
}

// ProviderCatalog 是一个提供商的上游模型清单。
type ProviderCatalog struct {
	Provider string
	Label    string
	Models   []CatalogModel
	// Source 是清单来源：remote / builtin / none。
	Source string
}

// ModelRow 是管理页的一行模型。
type ModelRow struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Provider       string   `json:"provider"`
	ProviderLabel  string   `json:"providerLabel"`
	Source         string   `json:"source"`
	Enabled        bool     `json:"enabled"`
	Aliases        []string `json:"aliases"`
	Capabilities   CapSet   `json:"capabilities"`
	CapOverrides   []string `json:"capOverrides"`
	SupportsTool   bool     `json:"supportsToolCall"`
	SupportsImages bool     `json:"supportsImages"`
	SupportsReason bool     `json:"supportsReasoning"`
	SupportsVideo  bool     `json:"supportsVideo"`
	IsCustom       bool     `json:"isCustom"`
}

// CapSet 是生效的能力位，nil 表示上游未声明。
type CapSet struct {
	MaxInputTokens    *int64 `json:"maxInputTokens"`
	MaxOutputTokens   *int64 `json:"maxOutputTokens"`
	SupportsToolCall  *bool  `json:"supportsToolCall"`
	SupportsImages    *bool  `json:"supportsImages"`
	SupportsVideo     *bool  `json:"supportsVideo"`
	SupportsReasoning *bool  `json:"supportsReasoning"`
}

// MappingRow 是管理页的一条映射。
type MappingRow struct {
	Alias     string `json:"alias"`
	Target    string `json:"target"`
	Provider  string `json:"provider"`
	Reasoning string `json:"reasoning"`
	Enabled   bool   `json:"enabled"`
	IsDefault bool   `json:"isDefault"`
	Dangling  bool   `json:"dangling"`
}

// ManageView 是 GET /api/models/manage 的响应体。
type ManageView struct {
	Models          []ModelRow        `json:"models"`
	Mappings        []MappingRow      `json:"mappings"`
	Providers       []ProviderSummary `json:"providers"`
	ReasoningLevels []string          `json:"reasoningLevels"`
}

// emptyView 保证 JSON 里是 [] 而不是 null —— 前端直接 .map() 不会炸。
func emptyView() ManageView {
	return ManageView{
		Models: []ModelRow{}, Mappings: []MappingRow{}, Providers: []ProviderSummary{},
		ReasoningLevels: ReasoningLevels,
	}
}

// ProviderSummary 是左栏的分组信息。
type ProviderSummary struct {
	Provider      string `json:"provider"`
	ProviderLabel string `json:"providerLabel"`
	Count         int    `json:"count"`
	Source        string `json:"source"`
}

// BuildView 把上游清单与规则合并成管理页视图。语义对齐 Agent2API 的
// catalog::manage_view：每个模型行自带一条默认绑定（alias == target），
// 再挂上所有指向它的显式别名；不在任何模型行上的映射标为 dangling。
func BuildView(r *Rules, catalogs []ProviderCatalog) ManageView {
	view := emptyView()
	r.mu.RLock()
	snap := cloneSnapshot(r.snap)
	r.mu.RUnlock()

	// 索引：provider(小写) → target(小写) → 是否出现
	hosted := map[string]map[string]bool{}
	attached := make([]bool, len(snap.Mappings))
	claimed := map[string]bool{}

	for _, cat := range catalogs {
		provider := cat.Provider
		pl := strings.ToLower(provider)
		if hosted[pl] == nil {
			hosted[pl] = map[string]bool{}
		}
		source := cat.Source
		if source == "" {
			source = "builtin"
		}

		items := append([]CatalogModel(nil), cat.Models...)
		// 自定义模型并进清单（上游没声明，但用户登记过）。
		custom := map[string]bool{}
		for _, c := range snap.Custom {
			if !strings.EqualFold(c.Provider, provider) {
				continue
			}
			custom[strings.ToLower(c.ID)] = true
			exists := false
			for _, it := range items {
				if strings.EqualFold(it.ID, c.ID) {
					exists = true
					break
				}
			}
			if !exists {
				items = append(items, CatalogModel{ID: c.ID, Name: c.ID})
			}
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].ID < items[j].ID })

		view.Providers = append(view.Providers, ProviderSummary{
			Provider: provider, ProviderLabel: cat.Label,
			Count: len(items), Source: source,
		})

		for _, item := range items {
			key := pl + "\x00" + strings.ToLower(item.ID)
			if claimed[key] {
				continue // 同家同 id 的重复条目只画一行
			}
			claimed[key] = true
			hosted[pl][strings.ToLower(item.ID)] = true

			row := ModelRow{
				ID: item.ID, Name: firstNonEmpty(item.Name, item.ID),
				Provider: provider, ProviderLabel: cat.Label,
				Source: source, IsCustom: custom[strings.ToLower(item.ID)],
			}
			row.Enabled = !disabledIn(snap.Disabled, provider, item.ID)
			row.Capabilities = effectiveCaps(item, snap.Capabilities, provider, item.ID)
			row.CapOverrides = overriddenKeys(snap.Capabilities, provider, item.ID)
			row.SupportsTool = boolOr(row.Capabilities.SupportsToolCall, false)
			row.SupportsImages = boolOr(row.Capabilities.SupportsImages, false)
			row.SupportsReason = boolOr(row.Capabilities.SupportsReasoning, false)
			row.SupportsVideo = boolOr(row.Capabilities.SupportsVideo, false)

			// 默认绑定：alias == target。它跟着模型行存在，不可删除。
			def, hasDef := findMapping(snap.Mappings, provider, item.ID, item.ID)
			if !hasDef {
				def = Mapping{Alias: item.ID, Target: item.ID, Provider: provider, Enabled: row.Enabled}
			}
			row.Aliases = []string{}
			if def.Enabled && !strings.EqualFold(def.Alias, def.Target) {
				row.Aliases = append(row.Aliases, def.Alias)
			}

			for i, mp := range snap.Mappings {
				if !strings.EqualFold(mp.Target, item.ID) ||
					!strings.EqualFold(mp.Provider, provider) {
					continue
				}
				attached[i] = true
				if strings.EqualFold(mp.Alias, item.ID) {
					continue // 默认绑定，不重复列出
				}
				if mp.Enabled {
					row.Aliases = append(row.Aliases, mp.Alias)
				}
			}
			sort.Strings(row.Aliases)
			view.Models = append(view.Models, row)
		}
	}

	// 挂不上的映射：target 不在任何已加载清单里，或者所属家没有账号。
	for i, mp := range snap.Mappings {
		if attached[i] {
			continue
		}
		carried := false
		if ids, ok := hosted[strings.ToLower(mp.Provider)]; ok {
			carried = ids[strings.ToLower(mp.Target)]
		}
		view.Mappings = append(view.Mappings, MappingRow{
			Alias: mp.Alias, Target: mp.Target, Provider: mp.Provider,
			Reasoning: mp.Reasoning, Enabled: mp.Enabled,
			IsDefault: strings.EqualFold(mp.Alias, mp.Target), Dangling: !carried,
		})
	}
	// 能落到行上的映射放进对应模型行之后（保持顺序：先模型行的映射，再悬空项）。
	var attachedRows []MappingRow
	for i, mp := range snap.Mappings {
		if !attached[i] {
			continue
		}
		attachedRows = append(attachedRows, MappingRow{
			Alias: mp.Alias, Target: mp.Target, Provider: mp.Provider,
			Reasoning: mp.Reasoning, Enabled: mp.Enabled,
			IsDefault: strings.EqualFold(mp.Alias, mp.Target),
		})
	}
	// 注意 append(x, y...) 在两侧都为空时返回 nil，前端 .map 会炸；
	// 这里显式拼成非 nil 切片。
	merged := make([]MappingRow, 0, len(attachedRows)+len(view.Mappings))
	merged = append(merged, attachedRows...)
	merged = append(merged, view.Mappings...)
	view.Mappings = merged
	return view
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func disabledIn(entries []RuleEntry, provider, id string) bool {
	for _, e := range entries {
		if !strings.EqualFold(e.ID, id) {
			continue
		}
		// provider 为空 = 旧版全局规则，对所有家生效。
		if e.Provider == "" || strings.EqualFold(e.Provider, provider) {
			return true
		}
	}
	return false
}

func findMapping(all []Mapping, provider, alias, target string) (Mapping, bool) {
	for _, m := range all {
		if strings.EqualFold(m.Provider, provider) &&
			strings.EqualFold(m.Alias, alias) &&
			strings.EqualFold(m.Target, target) {
			return m, true
		}
	}
	return Mapping{}, false
}

// effectiveCaps 把上游声明与用户覆盖合成生效值。覆盖里的 null 保留为 nil
// （显式「未声明」），以便管理页区分「不知道」与「明确不支持」。
func effectiveCaps(item CatalogModel, overrides []CapabilityOverride, provider, id string) CapSet {
	set := CapSet{
		MaxInputTokens:    intPtr(item.ContextLength),
		MaxOutputTokens:   intPtr(item.MaxOutputTokens),
		SupportsToolCall:  boolPtr(item.SupportsTools),
		SupportsImages:    boolPtr(item.SupportsImages),
		SupportsVideo:     boolPtr(item.SupportsVideo),
		SupportsReasoning: boolPtr(item.SupportsReason),
	}
	for _, o := range overrides {
		if !strings.EqualFold(o.Provider, provider) || !strings.EqualFold(o.ID, id) {
			continue
		}
		for k, v := range o.Values {
			switch k {
			case CapMaxInputTokens:
				set.MaxInputTokens = toIntPtr(v)
			case CapMaxOutputTokens:
				set.MaxOutputTokens = toIntPtr(v)
			case CapSupportsToolCall:
				set.SupportsToolCall = toBoolPtr(v)
			case CapSupportsImages:
				set.SupportsImages = toBoolPtr(v)
			case CapSupportsVideo:
				set.SupportsVideo = toBoolPtr(v)
			case CapSupportsReasoning:
				set.SupportsReasoning = toBoolPtr(v)
			}
		}
	}
	return set
}

func overriddenKeys(overrides []CapabilityOverride, provider, id string) []string {
	for _, o := range overrides {
		if strings.EqualFold(o.Provider, provider) && strings.EqualFold(o.ID, id) {
			keys := make([]string, 0, len(o.Values))
			for _, k := range CapKeys {
				if _, ok := o.Values[k]; ok {
					keys = append(keys, k)
				}
			}
			return keys
		}
	}
	return []string{}
}

func intPtr(v int) *int64 {
	if v <= 0 {
		return nil
	}
	n := int64(v)
	return &n
}

func toIntPtr(v any) *int64 {
	if v == nil {
		return nil
	}
	f, ok := toFloat(v)
	if !ok {
		return nil
	}
	n := int64(f)
	return &n
}

func boolPtr(v bool) *bool { return &v }

func toBoolPtr(v any) *bool {
	if v == nil {
		return nil
	}
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}
