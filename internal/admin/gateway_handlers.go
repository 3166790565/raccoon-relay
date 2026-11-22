package admin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/3166790565/raccoon-relay/internal/modelmeta"
	"github.com/3166790565/raccoon-relay/internal/sanitize"
)

// 这几个端点与设置页共用同一份状态（gatewaySelf + relay 快照），
// 面板既可以走 /api/settings 部分更新，也可以单独调这里做「即时开关」。

// handleGetSanitize 返回脱敏开关、词表与可选提供商（GET /api/gateway/sanitize）。
func (p *Panel) handleGetSanitize(w http.ResponseWriter, r *http.Request) {
	gs := p.gatewaySettings()
	writeJSON(w, 200, map[string]any{
		"ok":              true,
		"enabled":         gs["sanitizeEnabled"],
		"rules":           gs["sanitizeRules"],
		"customRules":     gs["sanitizeCustomRules"],
		"providers":       gs["sanitizeProviders"],
		"providerOptions": p.sanitizeProviderOptions(),
		"defaultRules":    sanitizeDefaults(),
		"roles":           sanitize.Roles,
		"limits": map[string]any{
			"maxRules": sanitize.MaxRules, "maxNameChars": sanitize.MaxNameChars,
			"maxPatternChars": sanitize.MaxPatternChars, "maxReplacementChars": sanitize.MaxReplacementChars,
		},
	})
}

// handlePutSanitize 部分更新脱敏设置（PUT /api/gateway/sanitize）。
// body 形状与 /api/settings 里的同名键一致，便于前端复用。
func (p *Panel) handlePutSanitize(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled   *bool            `json:"enabled"`
		Rules     *[]sanitize.Rule `json:"rules"`
		Providers *[]string        `json:"providers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	patch := settingsBody{
		SanitizeEnabled:   body.Enabled,
		SanitizeRules:     body.Rules,
		SanitizeProviders: body.Providers,
	}
	if err := p.applyGatewaySettings(r.Context(), patch); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	p.handleGetSanitize(w, r)
}

// handleGetModelsDev 返回 models.dev 元数据开关与状态（GET /api/gateway/modelsdev）。
func (p *Panel) handleGetModelsDev(w http.ResponseWriter, r *http.Request) {
	gs := p.gatewaySettings()
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"enabled": gs["modelsDevEnabled"],
		"status": map[string]any{
			"source":        gs["modelsDevSource"],
			"updatedAt":     gs["modelsDevUpdatedAt"],
			"count":         gs["modelsDevCount"],
			"lastRefresh":   gs["modelsDevLastRefresh"],
			"lastRefreshAt": gs["modelsDevLastRefreshAt"],
		},
	})
}

// handleRefreshModelsDev 立即拉一次 models.dev（POST /api/gateway/modelsdev/refresh）。
//
// 永远返回 2xx：成败由 ok 表达（与「模型目录刷新」同一口径：跑完了但这次
// 没刷是常态，不是需要调用方解析异常的错误）。
func (p *Panel) handleRefreshModelsDev(w http.ResponseWriter, r *http.Request) {
	ok, message, count := p.refreshModelsDev(r.Context())
	status := modelmeta.CurrentStatus()
	writeJSON(w, 200, map[string]any{
		"ok":      ok,
		"message": message,
		"count":   count,
		"status": map[string]any{
			"source":    status.Source,
			"updatedAt": status.UpdatedAt,
			"count":     status.Count,
		},
	})
}

// RefreshModelsDev 是给后台定时任务用的刷新入口（不返回 HTTP）。
// 关闭了 models.dev 或上一次仍在进行时静默跳过。
func (p *Panel) RefreshModelsDev(ctx context.Context) {
	p.refreshModelsDev(ctx)
}
