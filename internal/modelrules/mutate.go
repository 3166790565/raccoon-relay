package modelrules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// Error 是写操作的校验错误，带 HTTP 状态码供管理面直接回给前端。
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func badRequest(format string, args ...any) *Error {
	return &Error{Status: 400, Message: fmt.Sprintf(format, args...)}
}

// Err 让管理面统一判定：*Error 原样回，其余按 500。
func AsError(err error) *Error {
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Status: 500, Message: err.Error()}
}

// Manager 把规则快照与存储绑在一起：每次写操作都落库并替换快照。
type Manager struct {
	ST    *store.Store
	Rules *Rules
}

// NewManager 从存储加载规则。
func NewManager(ctx context.Context, st *store.Store) *Manager {
	return &Manager{ST: st, Rules: New(ctx, st)}
}

// persist 把快照写回 settings，成功后原子替换进程内快照。
func (m *Manager) persist(ctx context.Context, next snapshot) error {
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := m.ST.SetSetting(ctx, SettingKey, string(raw)); err != nil {
		return err
	}
	m.Rules.mu.Lock()
	m.Rules.snap = cloneSnapshot(next)
	m.Rules.mu.Unlock()
	return nil
}

/* ------------------------------------------------------------------ 启停 */

// SetState 启用/停用一个模型。provider 为空时按「对所有提供商生效」记录。
func (m *Manager) SetState(ctx context.Context, provider, id string, enabled bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return badRequest("缺少模型 id")
	}
	provider = strings.TrimSpace(provider)
	m.Rules.mu.RLock()
	next := cloneSnapshot(m.Rules.snap)
	m.Rules.mu.RUnlock()

	filtered := next.Disabled[:0]
	for _, e := range next.Disabled {
		if strings.EqualFold(e.ID, id) && strings.EqualFold(e.Provider, provider) {
			continue
		}
		filtered = append(filtered, e)
	}
	next.Disabled = filtered
	if !enabled {
		next.Disabled = append(next.Disabled, RuleEntry{Provider: provider, ID: id})
	}
	return m.persist(ctx, next)
}

/* ------------------------------------------------------------------ 映射 */

// AddMapping 新增或编辑一条映射。
//
// 三元组 (provider, alias, target) 相同即命中同一条：
//   - reasoning == nil（请求体没带这个键）→ 不动已有等级；
//   - reasoning 指向空串 → 显式清空；
//   - 其余 → 设为该值（表外自定义值放行，只做长度与空白校验）。
//
// create 为 true 时只允许新增，false 时只允许编辑。
func (m *Manager) AddMapping(ctx context.Context, provider, alias, target string, reasoning *string, enabled *bool, create bool) error {
	provider = strings.TrimSpace(provider)
	alias = strings.TrimSpace(alias)
	target = strings.TrimSpace(target)
	if provider == "" {
		return badRequest("缺少提供商")
	}
	if alias == "" {
		return badRequest("缺少对外映射名")
	}
	if target == "" {
		return badRequest("缺少上游模型")
	}
	if len(alias) > 128 {
		return badRequest("对外映射名不能超过 128 个字符")
	}
	var level string
	if reasoning != nil {
		level = strings.TrimSpace(*reasoning)
		if len(level) > ReasoningMaxChars {
			return badRequest("思考等级不能超过 %d 个字符", ReasoningMaxChars)
		}
	}

	m.Rules.mu.RLock()
	next := cloneSnapshot(m.Rules.snap)
	m.Rules.mu.RUnlock()

	idx := -1
	for i, mp := range next.Mappings {
		if strings.EqualFold(mp.Provider, provider) &&
			strings.EqualFold(mp.Alias, alias) &&
			strings.EqualFold(mp.Target, target) {
			idx = i
			break
		}
	}
	if create && idx >= 0 {
		return badRequest("该映射已存在")
	}
	if !create && idx < 0 {
		return badRequest("要编辑的映射不存在")
	}
	if idx >= 0 {
		if reasoning != nil {
			next.Mappings[idx].Reasoning = level
		}
		if enabled != nil {
			next.Mappings[idx].Enabled = *enabled
		}
	} else {
		mp := Mapping{Alias: alias, Target: target, Provider: provider, Enabled: true}
		if reasoning != nil {
			mp.Reasoning = level
		}
		if enabled != nil {
			mp.Enabled = *enabled
		}
		next.Mappings = append(next.Mappings, mp)
	}
	return m.persist(ctx, next)
}

// RemoveMapping 删除一条映射。自映射（alias == target）不允许删除，
// 只能关闭 —— 它跟着模型行存在，删掉会让这一行失去开关。
func (m *Manager) RemoveMapping(ctx context.Context, provider, alias, target string) error {
	provider = strings.TrimSpace(provider)
	alias = strings.TrimSpace(alias)
	target = strings.TrimSpace(target)
	if provider == "" || alias == "" || target == "" {
		return badRequest("缺少 provider / alias / target")
	}
	if strings.EqualFold(alias, target) {
		return badRequest("默认映射不能删除，只能关闭")
	}
	m.Rules.mu.RLock()
	next := cloneSnapshot(m.Rules.snap)
	m.Rules.mu.RUnlock()

	kept := next.Mappings[:0]
	found := false
	for _, mp := range next.Mappings {
		if strings.EqualFold(mp.Provider, provider) &&
			strings.EqualFold(mp.Alias, alias) &&
			strings.EqualFold(mp.Target, target) {
			found = true
			continue
		}
		kept = append(kept, mp)
	}
	if !found {
		return badRequest("映射不存在")
	}
	next.Mappings = kept
	return m.persist(ctx, next)
}

/* -------------------------------------------------------------- 自定义模型 */

// AddCustom 登记一个上游清单里没有的模型。
func (m *Manager) AddCustom(ctx context.Context, provider, id string) error {
	provider = strings.TrimSpace(provider)
	id = strings.TrimSpace(id)
	if provider == "" || id == "" {
		return badRequest("缺少提供商或模型 id")
	}
	if len(id) > 128 {
		return badRequest("模型 id 不能超过 128 个字符")
	}
	m.Rules.mu.RLock()
	next := cloneSnapshot(m.Rules.snap)
	m.Rules.mu.RUnlock()

	for _, c := range next.Custom {
		if strings.EqualFold(c.Provider, provider) && strings.EqualFold(c.ID, id) {
			return badRequest("该自定义模型已存在")
		}
	}
	next.Custom = append(next.Custom, CustomModel{Provider: provider, ID: id})
	return m.persist(ctx, next)
}

// RemoveCustom 移除一条自定义模型登记。
func (m *Manager) RemoveCustom(ctx context.Context, provider, id string) error {
	provider = strings.TrimSpace(provider)
	id = strings.TrimSpace(id)
	m.Rules.mu.RLock()
	next := cloneSnapshot(m.Rules.snap)
	m.Rules.mu.RUnlock()

	kept := next.Custom[:0]
	found := false
	for _, c := range next.Custom {
		if strings.EqualFold(c.Provider, provider) && strings.EqualFold(c.ID, id) {
			found = true
			continue
		}
		kept = append(kept, c)
	}
	if !found {
		return badRequest("自定义模型不存在")
	}
	next.Custom = kept
	return m.persist(ctx, next)
}

/* -------------------------------------------------------------- 能力位覆盖 */

// SetCapabilities 覆盖某个模型对下游声明的能力位。
// values 为 nil 表示清空该模型的全部覆盖；否则只合并给出的键，
// 值为 null 表示显式「未声明」。
func (m *Manager) SetCapabilities(ctx context.Context, provider, id string, values map[string]any) error {
	provider = strings.TrimSpace(provider)
	id = strings.TrimSpace(id)
	if provider == "" || id == "" {
		return badRequest("缺少提供商或模型 id")
	}
	normalized, err := NormalizeCapabilities(values)
	if err != nil {
		return err
	}
	m.Rules.mu.RLock()
	next := cloneSnapshot(m.Rules.snap)
	m.Rules.mu.RUnlock()

	idx := -1
	for i, o := range next.Capabilities {
		if strings.EqualFold(o.Provider, provider) && strings.EqualFold(o.ID, id) {
			idx = i
			break
		}
	}
	if len(normalized) == 0 {
		if idx >= 0 {
			next.Capabilities = append(next.Capabilities[:idx], next.Capabilities[idx+1:]...)
		}
		return m.persist(ctx, next)
	}
	if idx >= 0 {
		merged := next.Capabilities[idx].Values
		for k, v := range normalized {
			merged[k] = v
		}
		next.Capabilities[idx].Values = merged
	} else {
		next.Capabilities = append(next.Capabilities, CapabilityOverride{
			Provider: provider, ID: id, Values: normalized,
		})
	}
	return m.persist(ctx, next)
}

// NormalizeCapabilities 校验并归一六个能力键。未知键与非法值返回 400，
// 而不是静默丢弃 —— 管理页的输入错误应当当场可见。
func NormalizeCapabilities(values map[string]any) (map[string]any, error) {
	if values == nil {
		return nil, nil
	}
	out := make(map[string]any, len(values))
	for k, v := range values {
		switch k {
		case CapMaxInputTokens, CapMaxOutputTokens:
			if v == nil {
				out[k] = nil
				continue
			}
			f, ok := toFloat(v)
			if !ok || f < 1 || f > maxTokenValue {
				return nil, badRequest("%s 必须是 1 ~ %d 之间的正整数", k, maxTokenValue)
			}
			out[k] = int64(f)
		case CapSupportsToolCall, CapSupportsImages, CapSupportsVideo, CapSupportsReasoning:
			if v == nil {
				out[k] = nil
				continue
			}
			b, ok := v.(bool)
			if !ok {
				return nil, badRequest("%s 必须是布尔值或 null", k)
			}
			out[k] = b
		default:
			return nil, badRequest("未知的能力键: %s", k)
		}
	}
	return out, nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
