package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/modelmeta"
	"github.com/3166790565/raccoon-relay/internal/modelrules"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/sanitize"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// 设置键名（统一库 settings 表）。
const (
	keySanitizeEnabled     = "sanitizeEnabled"
	keySanitizeRules       = "sanitizeRules"
	keySanitizeProviders   = "sanitizeProviders"
	keyDeveloperRoleCompat = "developerRoleCompat"
	keyModelsDevEnabled    = "modelsDevEnabled"
	// keyModelsDevCache 存上次成功刷新的索引（重启后优先于内嵌快照）。
	keyModelsDevCache = "modelsDevCatalog"
)

// maxSanitizeProviders 限制应用范围的数量（防止误贴一大串）。
const maxSanitizeProviders = 200

// modelsDevInterval 是后台自动刷新的节奏。内嵌快照是发版那一刻的数据，
// 社区库的新模型与档位调整都在持续发生；与「模型目录刷新」同一量级即可。
const modelsDevInterval = 24 * time.Hour

// gatewaySelf 保存进程内生效的出站策略与元数据状态。ApplyStoredSettings 装载
// 初始值，面板保存时热更新；relay 侧另有快照（publishGatewayPolicy 同步）。
type gatewaySelf struct {
	mu            sync.Mutex
	sanitizeOn    bool
	rules         *sanitize.RuleSet
	customRules   bool
	providers     []string
	roleCompat    bool
	modelsDev     bool
	metaFetch     modelmeta.FetchFunc
	refreshing    bool
	lastRefresh   string
	lastRefreshAt int64
}

// ModelsDevInterval 返回后台自动刷新的间隔（装配层据此排期）。
func ModelsDevInterval() time.Duration { return modelsDevInterval }

func (p *Panel) gateway() *gatewaySelf {
	if p.gatewayState == nil {
		p.gatewayState = &gatewaySelf{rules: sanitize.DefaultRuleSet()}
	}
	return p.gatewayState
}

// SetMetaFetch 注入 models.dev 的出网函数（测试用；nil = 直连）。
func (p *Panel) SetMetaFetch(fn modelmeta.FetchFunc) {
	g := p.gateway()
	g.mu.Lock()
	g.metaFetch = fn
	g.mu.Unlock()
}

// gatewaySettings 返回当前生效的出站策略，供 GET /api/settings。
func (p *Panel) gatewaySettings() map[string]any {
	g := p.gateway()
	g.mu.Lock()
	sanitizeOn, roleCompat, modelsDev := g.sanitizeOn, g.roleCompat, g.modelsDev
	providers := append([]string(nil), g.providers...)
	rules := g.rules
	customRules := g.customRules
	lastRefresh, lastRefreshAt := g.lastRefresh, g.lastRefreshAt
	g.mu.Unlock()
	if rules == nil {
		rules = sanitize.DefaultRuleSet()
	}
	status := modelmeta.CurrentStatus()
	return map[string]any{
		"sanitizeEnabled":        sanitizeOn,
		"sanitizeProviders":      providers,
		"sanitizeRules":          rules.Entries(),
		"sanitizeCustomRules":    customRules,
		"developerRoleCompat":    roleCompat,
		"modelsDevEnabled":       modelsDev,
		"modelsDevSource":        status.Source,
		"modelsDevUpdatedAt":     status.UpdatedAt,
		"modelsDevCount":         status.Count,
		"modelsDevLastRefresh":   lastRefresh,
		"modelsDevLastRefreshAt": lastRefreshAt,
	}
}

// sanitizeDefaults 返回内置词表（面板「恢复内置词表」按钮展示用）。
func sanitizeDefaults() []sanitize.Rule {
	return sanitize.DefaultRuleSet().Entries()
}

// applyStoredGatewaySettings 把持久化值盖回进程并重建 relay 策略快照。
func (p *Panel) applyStoredGatewaySettings(ctx context.Context) {
	g := p.gateway()
	g.mu.Lock()
	g.sanitizeOn = readBoolSetting(ctx, p.st, keySanitizeEnabled, p.cfg.SanitizeEnabled)
	g.roleCompat = readBoolSetting(ctx, p.st, keyDeveloperRoleCompat, p.cfg.DeveloperRoleCompat)
	g.modelsDev = readBoolSetting(ctx, p.st, keyModelsDevEnabled, p.cfg.ModelsDevEnabled)

	// 提供商范围：库里空串/空数组 = 「全部提供商」（这是合法的显式配置，
	// 不是「没设置过」）；只有键不存在时才用环境变量给的默认值。
	providers := p.cfg.SanitizeProviders
	if raw, ok := p.st.GetSettingOK(ctx, keySanitizeProviders); ok {
		providers = nil
		if strings.TrimSpace(raw) != "" {
			var list []string
			if json.Unmarshal([]byte(raw), &list) == nil {
				providers = normalizeProviderList(list)
			}
		}
	}
	g.providers = providers

	// 词表：库里有一份就用库里的（自定义），否则用内置。
	// 空串表示「显式恢复内置」，同样回到出厂词表。
	g.rules = sanitize.DefaultRuleSet()
	g.customRules = false
	if raw, ok := p.st.GetSettingOK(ctx, keySanitizeRules); ok && strings.TrimSpace(raw) != "" {
		var entries []sanitize.Rule
		if json.Unmarshal([]byte(raw), &entries) == nil {
			if compiled, err := sanitize.Compile(entries); err == nil {
				g.rules = compiled
				g.customRules = true
			}
		}
	}
	g.mu.Unlock()

	// models.dev：库里上一份刷新结果优先于内嵌快照（重启不丢）。
	if raw, ok := p.st.GetSettingOK(ctx, keyModelsDevCache); ok && strings.TrimSpace(raw) != "" {
		if snapshot := modelmeta.Parse([]byte(raw), "cache"); snapshot != nil {
			modelmeta.Install(snapshot)
		}
	} else {
		modelmeta.Install(modelmeta.LoadEmbedded())
	}
	p.publishGatewayPolicy()
}

// publishGatewayPolicy 把当前出站策略与元数据查询入口推给数据面。
//
// 两个快照（relay 的出站开关、modelrules 的元数据视图）在同一处生成，
// 避免刷新元数据后出现「面板看到新数据、/v1/models 还是旧数据」的分叉。
func (p *Panel) publishGatewayPolicy() {
	g := p.gateway()
	g.mu.Lock()
	policy := relay.GatewayPolicy{
		SanitizeOn:          g.sanitizeOn,
		SanitizeProviders:   append([]string(nil), g.providers...),
		SanitizeRules:       g.rules,
		DeveloperRoleCompat: g.roleCompat,
		ModelsDev:           g.modelsDev,
	}
	g.mu.Unlock()

	status := modelmeta.CurrentStatus()
	lookupRelay := func(model string) *relay.MetaRecord {
		rec, ok := modelmeta.Lookup(model)
		if !ok {
			return nil
		}
		return &relay.MetaRecord{
			Reasoning: rec.Reasoning, ToolCall: rec.ToolCall, Images: rec.Images,
			Video: rec.Video, Context: rec.Context, Output: rec.Output, Levels: rec.Levels,
		}
	}
	lookupRules := func(model string) *modelrules.MetaRecord {
		rec, ok := modelmeta.Lookup(model)
		if !ok {
			return nil
		}
		return &modelrules.MetaRecord{
			Reasoning: rec.Reasoning, ToolCall: rec.ToolCall, Images: rec.Images,
			Video: rec.Video, Context: rec.Context, Output: rec.Output, Levels: rec.Levels,
		}
	}
	if status.Count > 0 {
		policy.MetaLookup = lookupRelay
	}
	relay.SetGatewayPolicy(policy)

	if p.modelRules != nil {
		if status.Count > 0 {
			p.modelRules.Rules.SetMeta(&modelrules.MetaInfo{
				Source: status.Source, UpdatedAt: status.UpdatedAt,
				Count: status.Count, Enabled: policy.ModelsDev, Lookup: lookupRules,
			})
		} else {
			p.modelRules.Rules.SetMeta(nil)
		}
	}
	if p.srv != nil {
		if policy.ModelsDev && status.Count > 0 {
			p.srv.SetCapabilityMeta(lookupRelay)
		} else {
			p.srv.SetCapabilityMeta(nil)
		}
	}
}

// applyGatewaySettings 校验并持久化出站策略。
//
// 部分更新：只处理请求体里显式给出的键。词表先整体编译，任何一条非法都
// 整条拒绝，不会「存一半、编译一半」——与其它设置卡片同一取舍。
func (p *Panel) applyGatewaySettings(ctx context.Context, body settingsBody) error {
	// 先把要落地的值算出来（全部校验通过才动内存与库）。
	var (
		nextOn        *bool
		nextRole      *bool
		nextModelsDev *bool
		nextProviders *[]string
		nextRules     *sanitize.RuleSet
		clearRules    bool
	)
	if body.SanitizeEnabled != nil {
		nextOn = body.SanitizeEnabled
	}
	if body.DeveloperRoleCompat != nil {
		nextRole = body.DeveloperRoleCompat
	}
	if body.ModelsDevEnabled != nil {
		nextModelsDev = body.ModelsDevEnabled
	}
	if body.SanitizeProviders != nil {
		list := normalizeProviderList(*body.SanitizeProviders)
		if len(list) > maxSanitizeProviders {
			return fmt.Errorf("应用提供商最多 %d 个", maxSanitizeProviders)
		}
		nextProviders = &list
	}
	// 词表三态：未给 = 不动；null / [] = 恢复内置；数组 = 自定义。
	if body.SanitizeRules != nil {
		if len(*body.SanitizeRules) == 0 {
			clearRules = true
		} else {
			compiled, err := sanitize.Compile(*body.SanitizeRules)
			if err != nil {
				return err
			}
			nextRules = compiled
		}
	}

	g := p.gateway()
	g.mu.Lock()
	if nextOn != nil {
		g.sanitizeOn = *nextOn
	}
	if nextRole != nil {
		g.roleCompat = *nextRole
	}
	if nextModelsDev != nil {
		g.modelsDev = *nextModelsDev
	}
	if nextProviders != nil {
		g.providers = *nextProviders
	}
	if nextRules != nil {
		g.rules = nextRules
		g.customRules = true
	}
	if clearRules {
		g.rules = sanitize.DefaultRuleSet()
		g.customRules = false
	}
	g.mu.Unlock()

	// 持久化。写库失败时内存已生效（与其它设置项同一取向）：返回错误让面板
	// 如实显示，而不是假装成功。
	if nextOn != nil {
		if err := p.st.SetSetting(ctx, keySanitizeEnabled, strconv.FormatBool(*nextOn)); err != nil {
			return err
		}
		p.event(store.ScopeRelay, "出站指纹脱敏", "已"+onOff(*nextOn))
	}
	if nextRole != nil {
		if err := p.st.SetSetting(ctx, keyDeveloperRoleCompat, strconv.FormatBool(*nextRole)); err != nil {
			return err
		}
		p.event(store.ScopeRelay, "开发者角色兼容", "已"+onOff(*nextRole))
	}
	if nextModelsDev != nil {
		if err := p.st.SetSetting(ctx, keyModelsDevEnabled, strconv.FormatBool(*nextModelsDev)); err != nil {
			return err
		}
		p.event(store.ScopeRelay, "models.dev 元数据", "已"+onOff(*nextModelsDev)+"（关闭只停联网刷新，现有数据继续用）")
	}
	if nextProviders != nil {
		raw, _ := json.Marshal(*nextProviders)
		if err := p.st.SetSetting(ctx, keySanitizeProviders, string(raw)); err != nil {
			return err
		}
	}
	if nextRules != nil {
		raw, _ := json.Marshal(nextRules.Entries())
		if err := p.st.SetSetting(ctx, keySanitizeRules, string(raw)); err != nil {
			return err
		}
	}
	if clearRules {
		if err := p.st.SetSetting(ctx, keySanitizeRules, ""); err != nil {
			return err
		}
	}

	p.publishGatewayPolicy()
	return nil
}

// refreshModelsDev 立即拉一次 models.dev 并落地。永远返回 2xx：成败由返回体
// 的 ok 表达（与「模型目录刷新」同一口径：跑完但这次没刷是常态）。
func (p *Panel) refreshModelsDev(ctx context.Context) (bool, string, int) {
	g := p.gateway()
	g.mu.Lock()
	if !g.modelsDev {
		g.mu.Unlock()
		return false, "已在设置里关闭 models.dev 模型元数据", 0
	}
	if g.refreshing {
		g.mu.Unlock()
		return false, "上一次刷新还在进行中", 0
	}
	g.refreshing = true
	fetch := g.metaFetch
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		g.refreshing = false
		g.mu.Unlock()
	}()

	refreshCtx, cancel := context.WithTimeout(ctx, modelmeta.RequestTimeout+5*time.Second)
	defer cancel()
	snapshot, err := modelmeta.Refresh(refreshCtx, fetch)
	if err != nil {
		msg := err.Error()
		g.mu.Lock()
		g.lastRefresh = "失败：" + msg
		g.lastRefreshAt = time.Now().UnixMilli()
		g.mu.Unlock()
		p.event(store.ScopeRelay, "models.dev 元数据刷新失败", msg)
		return false, msg, 0
	}
	modelmeta.Install(snapshot)
	_ = p.st.SetSetting(ctx, keyModelsDevCache, string(modelmeta.Serialize(snapshot)))
	p.publishGatewayPolicy()
	msg := fmt.Sprintf("models.dev 元数据已更新（%d 条）", snapshot.Len())
	g.mu.Lock()
	g.lastRefresh = "成功"
	g.lastRefreshAt = time.Now().UnixMilli()
	g.mu.Unlock()
	p.event(store.ScopeRelay, "models.dev 元数据已更新", msg)
	return true, msg, snapshot.Len()
}

// sanitizeProviderOptions 返回可选的提供商清单（面板多选）。
func (p *Panel) sanitizeProviderOptions() []map[string]any {
	out := []map[string]any{
		{"id": store.ProviderRaccoon, "label": "小浣熊"},
		{"id": store.ProviderSenseNova, "label": "日日新"},
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i]["id"].(string) < out[j]["id"].(string)
	})
	return out
}

// normalizeProviderList 去空白、小写、去重、排序。
func normalizeProviderList(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// readBoolSetting 读取布尔设置；库里没有就用默认值。
func readBoolSetting(ctx context.Context, st *store.Store, key string, def bool) bool {
	raw, ok := st.GetSettingOK(ctx, key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return b
}

func onOff(on bool) string {
	if on {
		return "开启"
	}
	return "关闭"
}

// p.event 写一条运行事件（失败静默：事件写入不该阻断设置保存）。
func (p *Panel) event(scope, message, detail string) {
	if p.st == nil {
		return
	}
	_ = p.st.AddEvent(context.Background(), store.RuntimeEvent{
		Level: store.LevelInfo, Scope: scope, Component: "settings",
		Message: message, Detail: detail,
	})
}
