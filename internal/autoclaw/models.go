package autoclaw

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Model 是 AutoClaw 的一个可调模型。上游用 routeModelId 作为模型名，
// 前缀表示来源：zai_ 智谱、zaicoding_ 编程套餐。
type Model struct {
	// RouteID 是发给上游的 model 字段（例如 zai_auto）。
	RouteID string
	Name    string
	Context int
	Output  int
	Vision  bool
}

func (m Model) PublicID() string { return store.ProviderAutoClaw + "/" + m.RouteID }

// FallbackModels 是国际站的内置模型快照：没有可用的实时清单时兜底，
// 也用于 /v1/models 让客户端在导入账号前就能配置。
func FallbackModels(station string) []Model {
	if station == store.StationCN {
		return managedModels()
	}
	return overseasModels()
}

func overseasModels() []Model {
	return []Model{
		{RouteID: "zai_auto", Name: "GLM Auto (路由)", Context: 200000, Output: 32000},
		{RouteID: "zai_auto-fast", Name: "GLM Auto Fast", Context: 200000, Output: 32000},
		{RouteID: "zaicoding_glm-5.3", Name: "GLM-5.3 Coding", Context: 1000000, Output: 131072},
		{RouteID: "zai_glm-5.3-flash", Name: "GLM-5.3 Flash", Context: 1000000, Output: 32000},
	}
}

func managedModels() []Model {
	return []Model{
		{RouteID: "zai_auto", Name: "Auto", Context: 1048576, Output: 393216, Vision: true},
		{RouteID: "zai_auto-fast", Name: "Auto-Fast", Context: 1048576, Output: 131072, Vision: true},
		{RouteID: "zaicoding_glm-5.3", Name: "GLM-5.3", Context: 1048576, Output: 307200},
		{RouteID: "zai_glm-5.3-flash", Name: "GLM-5.3-Flash", Context: 1048576, Output: 131072, Vision: true},
	}
}

// ToRaccoon 转成转发层的模型形状，ID 带对外前缀。
func ToRaccoon(list []Model) []raccoon.Model {
	out := make([]raccoon.Model, 0, len(list))
	for _, m := range list {
		out = append(out, raccoon.Model{
			ID:              m.PublicID(),
			Name:            m.Name,
			ContextLength:   m.Context,
			MaxOutputTokens: m.Output,
			SupportsImages:  m.Vision,
			SupportsTools:   true,
			// 三条 routeModelId 都支持思考（reasoning variants）。
			SupportsReason: true,
		})
	}
	return out
}

// catalogEntry 是「模型配置」接口的一条记录。上游返回的是不带 {code,data}
// 信封的裸 JSON：{"models":[...]}。
type catalogEntry struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	RouteID     string          `json:"routeId"`
	Context     json.RawMessage `json:"contextWindow"`
	MaxTokens   json.RawMessage `json:"maxTokens"`
	Input       []string        `json:"input"`
	Reasoning   *bool           `json:"reasoning"`
	CreditLevel json.RawMessage `json:"creditConsumptionLevel"`
}

// FetchModels 拉实时模型配置，失败返回 nil（调用方回落内置快照）。
//
// 配置地址由账号站点决定：上游把模型配置放在业务 host 的
// /autoclaw-proxy/proxy/autoclaw-model-config。
func (c *Client) FetchModels(ctx context.Context, acc *store.Account) []Model {
	cred, err := DecodeCred(acc)
	if err != nil || cred.Access == "" {
		return nil
	}
	env, err := c.doBusiness(ctx, c.hostFor(acc), http.MethodGet,
		"/autoclaw-proxy/proxy/autoclaw-model-config", cred.Access, nil)
	if err != nil {
		return nil
	}
	// 成功时 data 可能直接是 {"models":[...]}，也可能被包了一层。
	var payload struct {
		Models []catalogEntry `json:"models"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &payload)
	}
	if len(payload.Models) == 0 {
		return nil
	}
	out := make([]Model, 0, len(payload.Models))
	for _, e := range payload.Models {
		route := firstNonEmpty(e.RouteID, e.ID)
		if route == "" {
			continue
		}
		m := Model{
			RouteID: route,
			Name:    firstNonEmpty(strings.TrimSpace(e.Name), route),
			Context: int(numOf(jsonValue(e.Context))),
			Output:  int(numOf(jsonValue(e.MaxTokens))),
		}
		for _, in := range e.Input {
			if strings.Contains(strings.ToLower(in), "image") {
				m.Vision = true
			}
		}
		if m.Context == 0 {
			m.Context = 200000
		}
		if m.Output == 0 {
			m.Output = 32000
		}
		out = append(out, m)
	}
	return out
}

// ResolveRoute 把客户端写下的模型名映射到上游 routeModelId。
//
// 客户端的写法可能是：对外名 autoclaw/zai_auto、裸 route id zai_auto、
// 显示名 GLM Auto、甚至一个名字（glm-5.3-flash）。顺序与插件一致：
// 精确命中清单 → 已知前缀直接当 route → 形似 route 的直接用 → 否则 zai_auto。
func ResolveRoute(model string, catalog []Model) string {
	raw := strings.TrimSpace(model)
	raw = stripOwnPrefix(raw)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "zai_auto"
	}
	lower := strings.ToLower(raw)
	for _, m := range catalog {
		if lower == strings.ToLower(m.RouteID) || lower == strings.ToLower(m.Name) {
			return m.RouteID
		}
	}
	for _, prefix := range []string{"zaicoding_", "zai_"} {
		if strings.HasPrefix(lower, prefix) {
			return raw
		}
	}
	if looksLikeRouteID(raw) {
		return raw
	}
	return "zai_auto"
}

func looksLikeRouteID(s string) bool {
	i := strings.IndexByte(s, '_')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	for _, r := range s[:i] {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}
