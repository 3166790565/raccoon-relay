// Package modelrules 是「模型管理」的规则层：启停、别名映射、自定义模型、
// 能力位覆盖、思考等级绑定。数据面（/v1/models 对外清单与转发）与管理面
// （面板的模型页）共用这一份状态。
//
// 语义对齐 Agent2API（desktop-tauri/server/core/model_rules）：
//
//   - 启停（disabled）：按 (provider, id) 记录「对下游隐藏」的模型。
//     删除规则 = 恢复启用，所以开关是可逆的。
//   - 映射（mappings）：`alias → target`，让客户端用一个稳定的对外名，
//     后端随时换到另一个上游模型。三元组 (provider, alias, target) 唯一。
//     自映射（alias == target）是模型行的默认绑定，跟着行存在。
//   - 自定义模型（custom）：往上游清单里补一个它没声明、但实际可用的 id。
//   - 能力位覆盖（capabilities）：纠正上游声明的 maxInputTokens /
//     maxOutputTokens / supportsToolCall / supportsImages / supportsVideo /
//     supportsReasoning，六个键，null 表示「未声明」。
//   - 思考等级（mapping.reasoning）：挂在那条映射上的默认思考档位，
//     客户端显式指定时不覆盖。
//
// 全部状态落在 settings 表的一个键里，进程内带一份快照供转发热路径读取。
// 写操作走 Set* 方法，写完立即替换快照，多副本下以数据库为准（最后写入生效）。
package modelrules

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// SettingKey 是规则快照在 settings 表里的键。
const SettingKey = "model_rules"

// 六个能力键，顺序即管理页与能力徽章的展示顺序。
const (
	CapMaxInputTokens    = "maxInputTokens"
	CapMaxOutputTokens   = "maxOutputTokens"
	CapSupportsToolCall  = "supportsToolCall"
	CapSupportsImages    = "supportsImages"
	CapSupportsVideo     = "supportsVideo"
	CapSupportsReasoning = "supportsReasoning"
)

// CapKeys 是六个能力键的固定顺序。
var CapKeys = []string{
	CapMaxInputTokens, CapMaxOutputTokens, CapSupportsToolCall,
	CapSupportsImages, CapSupportsVideo, CapSupportsReasoning,
}

// ReasoningLevels 是思考等级的通用候选表（照抄 OmniProxy 的
// GENERIC_REASONING_LEVELS，与 Agent2API 同一份）。界面从这里取下拉项，
// 同时允许表外的自定义值。
var ReasoningLevels = []string{"off", "none", "minimal", "low", "medium", "high", "xhigh", "max"}

// ReasoningMaxChars 限制自定义思考等级的长度。
const ReasoningMaxChars = 32

// maxTokenValue 是 token 类能力键的上限（1 亿）。
const maxTokenValue = 100_000_000

// Mapping 是一条「对外名 → 上游模型」的映射。
type Mapping struct {
	Alias     string `json:"alias"`
	Target    string `json:"target"`
	Provider  string `json:"provider"`
	Reasoning string `json:"reasoning,omitempty"`
	Enabled   bool   `json:"enabled"`
}

// CustomModel 是用户手动登记的上游模型（清单补充）。
type CustomModel struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// CapabilityOverride 是某个 (provider, id) 的能力位覆盖。
// Values 里只出现被覆盖的键；null 显式表示「未声明」。
type CapabilityOverride struct {
	Provider string         `json:"provider"`
	ID       string         `json:"id"`
	Values   map[string]any `json:"values"`
}

// RuleEntry 是一条启停规则。
type RuleEntry struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// snapshot 是持久化的 JSON 形状。
type snapshot struct {
	Disabled     []RuleEntry          `json:"disabled"`
	Mappings     []Mapping            `json:"mappings"`
	Custom       []CustomModel        `json:"custom"`
	Capabilities []CapabilityOverride `json:"capabilities"`
}

// MetaRecord 是 models.dev 补进来的一条元数据（只留网关会用到的字段）。
type MetaRecord struct {
	Reasoning *bool
	ToolCall  *bool
	Images    *bool
	Video     *bool
	Context   *int64
	Output    *int64
	Levels    []string
}

// MetaInfo 是 models.dev 元数据的只读视图：当前快照的来源、条数与查询函数。
// 装配层注入（relay 不 import modelmeta，避免按需加载时多一层依赖）。
type MetaInfo struct {
	Source    string
	UpdatedAt int64
	Count     int
	Enabled   bool
	Lookup    func(model string) *MetaRecord
}

// Rules 是规则的进程内快照。读写都过 mu；读多写少，写时整份替换。
type Rules struct {
	mu   sync.RWMutex
	snap snapshot
	// meta 是 models.dev 元数据的查询入口（nil = 未接入，只显示上游声明）。
	meta *MetaInfo
}

// SetMeta 注入 models.dev 元数据查询入口。装配层与刷新链路调用。
func (r *Rules) SetMeta(meta *MetaInfo) {
	r.mu.Lock()
	r.meta = meta
	r.mu.Unlock()
}

// Meta 返回当前元数据视图（可能为 nil）。
func (r *Rules) Meta() *MetaInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.meta
}

// New 从存储加载规则。读不到或解析失败时返回空规则（管理页仍能打开）。
func New(ctx context.Context, st *store.Store) *Rules {
	r := &Rules{}
	raw, ok := st.GetSettingOK(ctx, SettingKey)
	if ok && strings.TrimSpace(raw) != "" {
		var s snapshot
		if json.Unmarshal([]byte(raw), &s) == nil {
			r.snap = s
		}
	}
	return r
}

// Disabled 判断某个 (provider, id) 是否被全局停用。
func (r *Rules) Disabled(provider, id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.snap.Disabled {
		if strings.EqualFold(e.Provider, provider) && strings.EqualFold(e.ID, id) {
			return true
		}
	}
	return false
}

// MappingFor 查一条映射（三元组精确命中，忽略大小写）。
func (r *Rules) MappingFor(provider, alias, target string) (Mapping, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.snap.Mappings {
		if strings.EqualFold(m.Provider, provider) &&
			strings.EqualFold(m.Alias, alias) &&
			strings.EqualFold(m.Target, target) {
			return m, true
		}
	}
	return Mapping{}, false
}

// Resolve 把客户端请求的模型名翻译成 (上游 provider, 上游模型, 思考等级)。
// 命中一条启用的映射时用它的 target；否则原样返回（provider 由调用方定）。
// 这是转发热路径，只读快照、不落库。
func (r *Rules) Resolve(model string) (target, reasoning string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	best := -1
	var hit Mapping
	for _, m := range r.snap.Mappings {
		if !m.Enabled || !strings.EqualFold(m.Alias, model) {
			continue
		}
		// 自映射优先级最低：同名的显式别名优先。
		score := 1
		if strings.EqualFold(m.Alias, m.Target) {
			score = 0
		}
		if score > best {
			best, hit = score, m
		}
	}
	if best < 0 {
		return "", "", false
	}
	return hit.Target, hit.Reasoning, true
}

// VisibleAliases 返回某个 (provider, target) 下所有启用的别名，供 /v1/models 广告。
func (r *Rules) VisibleAliases(provider, target string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, m := range r.snap.Mappings {
		if !m.Enabled || !strings.EqualFold(m.Provider, provider) ||
			!strings.EqualFold(m.Target, target) {
			continue
		}
		if strings.EqualFold(m.Alias, m.Target) {
			continue // 默认绑定就是模型本身，不单独广告
		}
		out = append(out, m.Alias)
	}
	sort.Strings(out)
	return out
}

// CustomModels 返回某个 provider 手动登记的模型 id。
func (r *Rules) CustomModels(provider string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, c := range r.snap.Custom {
		if strings.EqualFold(c.Provider, provider) {
			out = append(out, c.ID)
		}
	}
	return out
}

// CapabilityOverrides 返回某个 (provider, id) 的能力覆盖。
func (r *Rules) CapabilityOverrides(provider, id string) map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, o := range r.snap.Capabilities {
		if strings.EqualFold(o.Provider, provider) && strings.EqualFold(o.ID, id) {
			out := make(map[string]any, len(o.Values))
			for k, v := range o.Values {
				out[k] = v
			}
			return out
		}
	}
	return nil
}

// Snapshot 返回深拷贝，供管理页一次性取全部规则。
func (r *Rules) Snapshot() snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneSnapshot(r.snap)
}

// Clone 是 Snapshot 的公开形态（管理面用）。
func (r *Rules) Clone() Snapshot {
	s := r.Snapshot()
	return Snapshot{Disabled: s.Disabled, Mappings: s.Mappings, Custom: s.Custom, Capabilities: s.Capabilities}
}

// Snapshot 是管理面读取的全量规则。
type Snapshot struct {
	Disabled     []RuleEntry
	Mappings     []Mapping
	Custom       []CustomModel
	Capabilities []CapabilityOverride
}

func cloneSnapshot(s snapshot) snapshot {
	out := snapshot{
		Disabled:     append([]RuleEntry(nil), s.Disabled...),
		Mappings:     append([]Mapping(nil), s.Mappings...),
		Custom:       append([]CustomModel(nil), s.Custom...),
		Capabilities: make([]CapabilityOverride, 0, len(s.Capabilities)),
	}
	for _, o := range s.Capabilities {
		values := make(map[string]any, len(o.Values))
		for k, v := range o.Values {
			values[k] = v
		}
		out.Capabilities = append(out.Capabilities, CapabilityOverride{
			Provider: o.Provider, ID: o.ID, Values: values,
		})
	}
	return out
}

// Hidden 判断某个上游模型是否被停用。provider 维度在 /v1/models 这一层
// 无法一一对应（同一 id 可能在多家存在），因此这里按 id 判定：任意一家
// 显式停用它就算停用 —— 宁可少广告，也不让客户端请求一个已知不可用的名字。
func (r *Rules) Hidden(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.snap.Disabled {
		if strings.EqualFold(e.ID, id) {
			return true
		}
	}
	return false
}

// AliasesOf 返回所有启用、且指向该上游模型的显式别名（不含自映射）。
func (r *Rules) AliasesOf(target string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, m := range r.snap.Mappings {
		if !m.Enabled || !strings.EqualFold(m.Target, target) {
			continue
		}
		if strings.EqualFold(m.Alias, m.Target) {
			continue
		}
		out = append(out, m.Alias)
	}
	sort.Strings(out)
	return out
}
