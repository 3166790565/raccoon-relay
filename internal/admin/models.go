package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/modelrules"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/sensenova"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// SetModelCatalog 注入模型整理的依赖。装配层调用。
func (p *Panel) SetModelCatalog(cat *relay.Catalog, mgr *modelrules.Manager, sn *sensenova.Client) {
	p.modelCatalog = cat
	p.modelRules = mgr
	p.snClient = sn
}

// handleModelManage 是模型管理页的唯一读接口：返回合并了实时清单与规则的视图。
func (p *Panel) handleModelManage(w http.ResponseWriter, r *http.Request) {
	if p.modelRules == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "模型管理未启用"})
		return
	}
	view := modelrules.BuildView(p.modelRules.Rules, p.modelCatalogs(r))
	writeJSON(w, 200, map[string]any{"ok": true, "view": view})
}

// handleModelState 启停一个模型。
func (p *Panel) handleModelState(w http.ResponseWriter, r *http.Request) {
	if p.modelRules == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "模型管理未启用"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
		Enabled  *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if body.Enabled == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "缺少 enabled 字段"})
		return
	}
	if err := p.modelRules.SetState(r.Context(), body.Provider, body.ID, *body.Enabled); err != nil {
		writeModelRuleError(w, err)
		return
	}
	p.writeModelManage(w, r)
}

// handleModelMapping 新增 / 编辑映射。reasoning 与 enabled 都是三态：
// 请求体里没带 = 不动，带了就显式设置。
func (p *Panel) handleModelMapping(w http.ResponseWriter, r *http.Request) {
	if p.modelRules == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "模型管理未启用"})
		return
	}
	var body struct {
		Provider  string  `json:"provider"`
		Alias     string  `json:"alias"`
		Target    string  `json:"target"`
		Reasoning *string `json:"reasoning"`
		Enabled   *bool   `json:"enabled"`
		Create    bool    `json:"create"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := p.modelRules.AddMapping(r.Context(), body.Provider, body.Alias, body.Target,
		body.Reasoning, body.Enabled, body.Create); err != nil {
		writeModelRuleError(w, err)
		return
	}
	p.writeModelManage(w, r)
}

// handleModelMappingRemove 删除一条映射。
func (p *Panel) handleModelMappingRemove(w http.ResponseWriter, r *http.Request) {
	if p.modelRules == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "模型管理未启用"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Alias    string `json:"alias"`
		Target   string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := p.modelRules.RemoveMapping(r.Context(), body.Provider, body.Alias, body.Target); err != nil {
		writeModelRuleError(w, err)
		return
	}
	p.writeModelManage(w, r)
}

// handleModelCustom 登记一个上游清单里没有的模型。
func (p *Panel) handleModelCustom(w http.ResponseWriter, r *http.Request) {
	if p.modelRules == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "模型管理未启用"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := p.modelRules.AddCustom(r.Context(), body.Provider, body.ID); err != nil {
		writeModelRuleError(w, err)
		return
	}
	p.writeModelManage(w, r)
}

// handleModelCustomRemove 移除一条自定义模型登记。
func (p *Panel) handleModelCustomRemove(w http.ResponseWriter, r *http.Request) {
	if p.modelRules == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "模型管理未启用"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := p.modelRules.RemoveCustom(r.Context(), body.Provider, body.ID); err != nil {
		writeModelRuleError(w, err)
		return
	}
	p.writeModelManage(w, r)
}

// handleModelCapabilities 覆盖某个模型对下游声明的能力位。
func (p *Panel) handleModelCapabilities(w http.ResponseWriter, r *http.Request) {
	if p.modelRules == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "模型管理未启用"})
		return
	}
	var body struct {
		Provider     string         `json:"provider"`
		ID           string         `json:"id"`
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if err := p.modelRules.SetCapabilities(r.Context(), body.Provider, body.ID, body.Capabilities); err != nil {
		writeModelRuleError(w, err)
		return
	}
	p.writeModelManage(w, r)
}

// handleModelTest 用指定模型发一次最小请求，验证连通性。
// 走的是真实转发链路之外的一次直连探针，不写流水、不动账号状态。
func (p *Panel) handleModelTest(w http.ResponseWriter, r *http.Request) {
	if p.snClient == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "上游未配置"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if strings.EqualFold(body.Provider, "raccoon") {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "暂只支持日日新模型的连通性测试"})
		return
	}
	accounts, err := p.st.ListAccounts(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	var picked *store.Account
	for _, a := range accounts {
		if a.IsSenseNova() && a.Enabled && strings.TrimSpace(a.Token) != "" {
			picked = a
			break
		}
	}
	if picked == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "没有带 API Key 的日日新账号可用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	start := time.Now()
	models := p.snClient.LiveModels(ctx, picked.Token)
	if len(models) == 0 {
		writeJSON(w, 200, map[string]any{
			"ok": false, "message": "上游没有返回模型清单（Key 失效或额度耗尽）",
			"elapsedMs": time.Since(start).Milliseconds(),
		})
		return
	}
	found := ""
	for _, m := range models {
		if strings.EqualFold(m.ID, body.ID) {
			found = m.ID
			break
		}
	}
	if found == "" {
		writeJSON(w, 200, map[string]any{
			"ok": false, "message": "上游清单里没有这个模型",
			"elapsedMs": time.Since(start).Milliseconds(),
		})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "message": "模型可用", "elapsedMs": time.Since(start).Milliseconds(),
	})
}

// writeModelManage 写完规则后统一回最新视图，前端就地重绘。
func (p *Panel) writeModelManage(w http.ResponseWriter, r *http.Request) {
	view := modelrules.BuildView(p.modelRules.Rules, p.modelCatalogs(r))
	writeJSON(w, 200, map[string]any{"ok": true, "view": view})
}

// modelCatalogs 把上游实时清单包成模型页的中立形状，按供应商分组。
//
// 只读本地 Catalog 缓存，不触发上游请求（避免管理页每次刷新都打上游）。
// 小浣熊、日日新与外部渠道都要列出来：各自有独立的模型阵容和订阅品类，
// 少一家就会出现「号池里有号、模型页里看不到它家的模型」。
func (p *Panel) modelCatalogs(r *http.Request) []modelrules.ProviderCatalog {
	if p.modelCatalog == nil {
		return nil
	}
	live := p.modelCatalog.List(r.Context())
	var sn, rc []modelrules.CatalogModel
	for _, m := range live {
		item := modelrules.CatalogModel{
			ID:              m.ID,
			Name:            m.Name,
			ContextLength:   m.ContextLength,
			MaxOutputTokens: m.MaxOutputTokens,
			SupportsImages:  m.SupportsImages,
			SupportsTools:   m.SupportsTools,
			SupportsReason:  m.SupportsReason,
			// 实时清单里上游已给出的上下限就是「上游声明」。
			DeclaredContext:   int64(m.ContextLength),
			DeclaredMaxOutput: int64(m.MaxOutputTokens),
		}
		if sensenova.IsKnownModel(m.ID) {
			sn = append(sn, item)
		} else {
			rc = append(rc, item)
		}
	}
	out := make([]modelrules.ProviderCatalog, 0, 2)
	if len(rc) > 0 {
		out = append(out, modelrules.ProviderCatalog{
			Provider: store.ProviderRaccoon, Label: "小浣熊", Models: rc, Source: "remote",
		})
	}
	if len(sn) > 0 {
		out = append(out, modelrules.ProviderCatalog{
			Provider: store.ProviderSenseNova, Label: "日日新", Models: sn, Source: "remote",
		})
	}
	return out
}

func writeModelRuleError(w http.ResponseWriter, err error) {
	e := modelrules.AsError(err)
	writeJSON(w, e.Status, map[string]any{"ok": false, "message": e.Message})
}
