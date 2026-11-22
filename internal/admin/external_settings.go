package admin

import (
	"context"
	"net/url"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// extModels 是外部渠道模型清单的拉取函数（按账号）。nil 表示未接入。
func (p *Panel) externalModelSource() func(context.Context, *store.Account) []raccoon.Model {
	return p.extModels
}

// 外部渠道（AutoClaw / CodeArts）的上游基址设置。
//
// 这两家的地址在面板里改只有一个实际用途：上游改域名或自建代理时不用重新发版。
// 空值表示「用内置默认地址」，所以读取必须用 GetSettingOK —— 用 GetSetting
// 会把「存的是空串」和「从没设置过」混为一谈。
// extBaseKey 把「库里的键」「面板字段名」「cfg 字段地址」绑在一起。
// 地址是 *string，指向 p.cfg 的字段；在 apply* 里按 p.cfg 现取。
type extBaseKey struct {
	key   string
	camp  string // 面板上的字段名（GET /api/settings 的键）
	label string
	field func(p *Panel) *string
}

var extBaseKeys = []extBaseKey{
	{"autoclawIntlBase", "autoClawIntlBase", "AutoClaw 国际站",
		func(p *Panel) *string { return &p.cfg.AutoClawInternationalBase }},
	{"autoclawCNBase", "autoClawCNBase", "AutoClaw 国内站",
		func(p *Panel) *string { return &p.cfg.AutoClawCNBase }},
	{"autoclawCNManagedBase", "autoClawCNManagedBase", "AutoClaw 国内对话网关",
		func(p *Panel) *string { return &p.cfg.AutoClawCNManagedBase }},
	{"codeartsMainlandBase", "codeArtsMainlandBase", "码道国内站",
		func(p *Panel) *string { return &p.cfg.CodeArtsMainlandBase }},
	{"codeartsIntlBase", "codeArtsIntlBase", "码道国际站",
		func(p *Panel) *string { return &p.cfg.CodeArtsInternationalBase }},
}

// externalSettings 返回当前生效的外部渠道基址，供 GET /api/settings。
func (p *Panel) externalSettings() map[string]any {
	out := map[string]any{}
	for _, it := range extBaseKeys {
		out[it.camp] = *it.field(p)
	}
	return out
}

// applyStoredExternalSettings 把库里的基址盖回 cfg。
// 空串是合法值（= 用内置默认），所以用 GetSettingOK 而不是 GetSetting。
func (p *Panel) applyStoredExternalSettings(ctx context.Context) {
	for _, it := range extBaseKeys {
		raw, ok := p.st.GetSettingOK(ctx, it.key)
		if !ok {
			continue
		}
		*it.field(p) = raw
	}
}

// applyExternalSettings 校验并持久化外部渠道基址。
//
// 先整体校验再落地：五个地址里只要有一个非法，整条拒绝，避免存一半。
// 非法值包括：不是 http(s)、带用户信息、带查询串/片段。
func (p *Panel) applyExternalSettings(ctx context.Context, body settingsBody) error {
	vals := map[string]*string{
		"autoclawIntlBase":      body.AutoClawIntlBase,
		"autoclawCNBase":        body.AutoClawCNBase,
		"autoclawCNManagedBase": body.AutoClawCNManagedBase,
		"codeartsMainlandBase":  body.CodeArtsMainlandBase,
		"codeartsIntlBase":      body.CodeArtsIntlBase,
	}
	normalized := map[string]string{}
	for key, ptr := range vals {
		if ptr == nil {
			continue
		}
		v := strings.TrimRight(strings.TrimSpace(*ptr), "/")
		if v != "" {
			if err := validateBaseURL(v); err != nil {
				return err
			}
		}
		normalized[key] = v
	}
	// 全部合法后才写库与内存。
	for _, it := range extBaseKeys {
		v, ok := normalized[it.key]
		if !ok {
			continue
		}
		*it.field(p) = v
		if err := p.st.SetSetting(ctx, it.key, v); err != nil {
			return err
		}
	}
	return nil
}

// validateBaseURL 只接受干净的 http(s) 基址。
//
// 允许空串（= 默认值）。带查询串或片段的地址在拼路径时会出错，
// 带用户信息的地址一旦被日志打出来就是凭据泄漏，都直接拒绝。
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return &extSettingError{msg: "地址无法解析：" + raw}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return &extSettingError{msg: "地址必须以 http:// 或 https:// 开头：" + raw}
	}
	if u.Host == "" {
		return &extSettingError{msg: "地址缺少主机名：" + raw}
	}
	if u.User != nil {
		return &extSettingError{msg: "地址不能带用户名/密码：" + raw}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return &extSettingError{msg: "地址不能带查询串或片段：" + raw}
	}
	return nil
}

type extSettingError struct{ msg string }

func (e *extSettingError) Error() string { return e.msg }

// extUpstream 返回某外部渠道的转发实现（续期在它上面）。
func (p *Panel) extUpstream(provider string) relayExt {
	if p.relayExt == nil {
		return nil
	}
	return p.relayExt(provider)
}

// relayExt 是外部渠道在管理面需要的最小能力。
// relay.ExtUpstream 正好满足它，这里单独定义是为了不把 relay 的完整接口
// 泄漏进管理面的签名里。
type relayExt interface {
	EnsureCredential(ctx context.Context, acc *store.Account) error
}

// ExtUpstreamRef 是外部渠道在管理面需要的最小能力。
type ExtUpstreamRef = relayExt

// SetExternalModelSource 由装配层注入「按供应商拉实时模型清单」。
func (p *Panel) SetExternalModelSource(fn func(ctx context.Context, acc *store.Account) []raccoon.Model) {
	p.extModels = fn
}

// extModelSource 返回模型清单拉取函数；未配置返回 false。
func (p *Panel) extModelSource(provider string) (func(context.Context, *store.Account) []raccoon.Model, bool) {
	if p.extModels == nil {
		return nil, false
	}
	return p.extModels, true
}

// SetExternalUpstream 由装配层注入「按供应商取外部渠道实现」。
func (p *Panel) SetExternalUpstream(fn func(provider string) relayExt) {
	p.relayExt = fn
}

// extProviderLabel 是外部渠道在面板上的展示名。
func extProviderLabel(provider string) string {
	switch provider {
	case store.ProviderAutoClaw:
		return "AutoClaw"
	case store.ProviderCodeArts:
		return "码道 CodeArts"
	}
	return provider
}
