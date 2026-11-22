package modelrules

import (
	"sort"
	"strings"
)

// CatalogModel 是中立的模型条目形状。由装配层从日日新实时清单 / 内置快照
// 映射进来，避免本包依赖具体上游包。
//
// 带 *bool 的字段来自 tools/思考的上游声明，nil = 上游没声明（= 「未声明」）。
// 这与「上游明确说不支持」（false）是两件事：models.dev 只补前者的空位。
type CatalogModel struct {
	ID              string
	Name            string
	ContextLength   int
	MaxOutputTokens int
	SupportsImages  bool
	SupportsTools   bool
	SupportsReason  bool
	SupportsVideo   bool
	// 三态能力位：nil = 上游未声明，交由 models.dev 补齐。
	DeclaredTools     *bool
	DeclaredReason    *bool
	DeclaredImages    *bool
	DeclaredVideo     *bool
	DeclaredContext   int64
	DeclaredMaxOutput int64
	// ReasoningLevels 是上游自己声明的命名档位（空 = 未声明）。
	ReasoningLevels []string
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
	// CapSources 记录每个能力键的来源：upstream / modelsdev / user。
	// 管理页据此把「真实的」与「外部数据补的」分开显示 —— 没有这个标记，
	// 界面上的六个键看起来一样可信，而它们的可信度差着量级。
	CapSources map[string]string `json:"capSources,omitempty"`
	// ReasoningLevels 是该模型认识的命名思考档位（空 = 未声明，前端回落通用表）。
	ReasoningLevels []string `json:"reasoningLevels,omitempty"`
	// ReasoningSource 是档位的来源：upstream / modelsdev / undeclared。
	ReasoningSource string `json:"reasoningSource,omitempty"`
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
	// MetaSource 是 models.dev 元数据的来源与条数（管理页据此说明数据新旧）。
	MetaSource    string `json:"metaSource,omitempty"`
	MetaUpdatedAt int64  `json:"metaUpdatedAt,omitempty"`
	MetaCount     int    `json:"metaCount,omitempty"`
	// MetaEnabled 表示 models.dev 补齐是否开启（关闭后仍用现有数据）。
	MetaEnabled bool `json:"metaEnabled"`
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
	meta := r.meta
	r.mu.RUnlock()
	if meta != nil {
		view.MetaSource = meta.Source
		view.MetaUpdatedAt = meta.UpdatedAt
		view.MetaCount = meta.Count
		view.MetaEnabled = meta.Enabled
	}

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
			row.Capabilities, row.CapSources = effectiveCaps(item, snap.Capabilities, provider, item.ID, r.meta)
			row.CapOverrides = overriddenKeys(snap.Capabilities, provider, item.ID)
			row.SupportsTool = boolOr(row.Capabilities.SupportsToolCall, false)
			row.SupportsImages = boolOr(row.Capabilities.SupportsImages, false)
			row.SupportsReason = boolOr(row.Capabilities.SupportsReasoning, false)
			row.SupportsVideo = boolOr(row.Capabilities.SupportsVideo, false)
			row.ReasoningLevels, row.ReasoningSource = effectiveLevels(item, r.meta)

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

// effectiveCaps 把「上游声明 → models.dev → 用户覆盖」合成生效值，并给出每个
// 键的来源标记。
//
// 优先级与 Agent2API 一致：用户覆盖 > 上游声明 > models.dev > 未声明。
// models.dev 只补上游**没声明**的键（nil），上游说过的值一律不动。
// 覆盖里的 null 表示显式「未声明」，管理页据此区分「不知道」与「明确不支持」。
func effectiveCaps(item CatalogModel, overrides []CapabilityOverride, provider, id string, meta *MetaInfo) (CapSet, map[string]string) {
	var rec *MetaRecord
	if meta != nil && meta.Lookup != nil {
		rec = meta.Lookup(item.ID)
	}
	sources := map[string]string{}
	set := CapSet{}

	boolKey := func(key string, declared *bool, upstream bool, pick func(*MetaRecord) *bool) {
		if declared != nil {
			v := *declared
			setAssignBool(&set, key, &v)
			sources[key] = "upstream"
			return
		}
		if upstream {
			v := true
			setAssignBool(&set, key, &v)
			sources[key] = "upstream"
			return
		}
		if rec != nil && pick != nil && pick(rec) != nil {
			v := *pick(rec)
			setAssignBool(&set, key, &v)
			sources[key] = "modelsdev"
		}
	}
	tokenKey := func(key string, declared int64, upstream int, pick func(*MetaRecord) *int64) {
		if declared > 0 {
			v := declared
			setAssignToken(&set, key, &v)
			sources[key] = "upstream"
			return
		}
		if upstream > 0 {
			v := int64(upstream)
			setAssignToken(&set, key, &v)
			sources[key] = "upstream"
			return
		}
		if rec != nil && pick != nil && pick(rec) != nil {
			v := *pick(rec)
			setAssignToken(&set, key, &v)
			sources[key] = "modelsdev"
		}
	}

	// 上游真值：显式三态优先，没有三态时用旧布尔位（只有 true 能作数，
	// false 与「未声明」在旧形状里分不开，所以交给 models.dev 补）。
	boolKey(CapSupportsToolCall, item.DeclaredTools, item.SupportsTools, func(r *MetaRecord) *bool { return r.ToolCall })
	boolKey(CapSupportsImages, item.DeclaredImages, item.SupportsImages, func(r *MetaRecord) *bool { return r.Images })
	boolKey(CapSupportsVideo, item.DeclaredVideo, item.SupportsVideo, func(r *MetaRecord) *bool { return r.Video })
	boolKey(CapSupportsReasoning, item.DeclaredReason, item.SupportsReason, func(r *MetaRecord) *bool { return r.Reasoning })
	tokenKey(CapMaxInputTokens, item.DeclaredContext, item.ContextLength, func(r *MetaRecord) *int64 { return r.Context })
	tokenKey(CapMaxOutputTokens, item.DeclaredMaxOutput, item.MaxOutputTokens, func(r *MetaRecord) *int64 { return r.Output })

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
			default:
				continue
			}
			sources[k] = "user"
		}
	}
	if len(sources) == 0 {
		return set, nil
	}
	return set, sources
}

// effectiveLevels 归一逐模型思考档位：上游声明 > models.dev > 未声明。
// 都拿不到就不出这个键，前端据此回落通用档位表。
func effectiveLevels(item CatalogModel, meta *MetaInfo) ([]string, string) {
	if len(item.ReasoningLevels) > 0 {
		return append([]string(nil), item.ReasoningLevels...), "upstream"
	}
	if meta != nil && meta.Lookup != nil {
		if rec := meta.Lookup(item.ID); rec != nil && len(rec.Levels) > 0 {
			return append([]string(nil), rec.Levels...), "modelsdev"
		}
	}
	return nil, "undeclared"
}

func setAssignBool(set *CapSet, key string, v *bool) {
	switch key {
	case CapSupportsToolCall:
		set.SupportsToolCall = v
	case CapSupportsImages:
		set.SupportsImages = v
	case CapSupportsVideo:
		set.SupportsVideo = v
	case CapSupportsReasoning:
		set.SupportsReasoning = v
	}
}

func setAssignToken(set *CapSet, key string, v *int64) {
	switch key {
	case CapMaxInputTokens:
		set.MaxInputTokens = v
	case CapMaxOutputTokens:
		set.MaxOutputTokens = v
	}
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
