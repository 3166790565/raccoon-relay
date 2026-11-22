package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Model 是码道的一个可调模型。
type Model struct {
	ID      string // 上游真实 id（可能带日期后缀，如 deepseek-v4.1-flash-1017）
	Name    string
	Context int
	Output  int
	// Benefit 表示这是当日免费额度里的模型，调用时要带 maas_type: benefit。
	Benefit bool
}

func (m Model) PublicID() string { return store.ProviderCodeArts + "/" + m.ID }

// FallbackModels 是内置模型快照：实时清单拉不到时兜底。
func FallbackModels() []Model {
	return []Model{
		{ID: "GLM-5.2", Name: "GLM-5.2", Context: 202752, Output: 65536},
		{ID: "GLM-5.1", Name: "GLM-5.1", Context: 202752, Output: 65536},
		{ID: "glm-5.3-flash", Name: "GLM-5.3 Flash", Context: 1048576, Output: 65536},
		{ID: "openpangu-2.0-flash", Name: "openPangu 2.0 Flash", Context: 262144, Output: 65536},
		{ID: "openpangu-2.0-pro", Name: "openPangu 2.0 Pro", Context: 1048576, Output: 65536},
		{ID: "deepseek-v4-flash-0731", Name: "DeepSeek V4 Flash", Context: 1048576, Output: 65536},
		{ID: "deepseek-v4-pro-0813", Name: "DeepSeek V4 Pro", Context: 1048576, Output: 65536},
		{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Context: 1000000, Output: 65536},
	}
}

// ToRaccoon 转成转发层的模型形状，ID 带 codearts/ 前缀。
func ToRaccoon(list []Model) []raccoon.Model {
	out := make([]raccoon.Model, 0, len(list))
	for _, m := range list {
		out = append(out, raccoon.Model{
			ID:              m.PublicID(),
			Name:            m.Name,
			ContextLength:   m.Context,
			MaxOutputTokens: m.Output,
			// 码道对图片回 InferHub.001001020.406，所以一律声明为纯文本。
			SupportsImages: false,
			SupportsTools:  true,
			SupportsReason: true,
		})
	}
	return out
}

// builtinModel 是 /v1/model/builtin 的一条记录。
type builtinModel struct {
	ModelID       string `json:"model_id"`
	ModelName     string `json:"model_name"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	Enable        *bool  `json:"enable"`
}

// FetchModels 拉实时模型清单。失败返回 nil（调用方回落内置快照）。
//
// 码道没有统一的 /models：内置模型清单在 /v1/model/builtin，免费额度模型
// 在 opengw 的网关配置里。这里先取内置清单；免费模型由领用环节单独处理。
func (c *Client) FetchModels(ctx context.Context, acc *store.Account) []Model {
	cred, err := DecodeCred(acc)
	if err != nil {
		return nil
	}
	site := acc.Station()
	rawURL := c.snapOf(site) + "/v1/model/builtin"
	_, body, err := c.signedDo(ctx, cred, http.MethodGet, rawURL, nil,
		map[string]string{"content-type": "application/json"},
		map[string]string{"Agent-Type": "PromptCenter", "X-Language": "zh-cn"})
	if err != nil {
		return nil
	}
	var env struct {
		BuiltinModels []builtinModel `json:"builtinModels"`
		Data          struct {
			BuiltinModels []builtinModel `json:"builtinModels"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil {
		return nil
	}
	items := env.BuiltinModels
	if len(items) == 0 {
		items = env.Data.BuiltinModels
	}
	out := make([]Model, 0, len(items))
	for _, e := range items {
		if e.Enable != nil && !*e.Enable {
			continue
		}
		id := strings.TrimSpace(e.ModelID)
		if id == "" || isVisionModel(id) {
			continue
		}
		m := Model{ID: id, Name: firstNonEmpty(strings.TrimSpace(e.ModelName), id),
			Context: e.ContextWindow, Output: e.MaxTokens}
		if m.Context == 0 {
			m.Context = contextWindowOf(id)
		}
		if m.Output <= 0 || m.Output > maxOutputTokens {
			m.Output = maxOutputTokens
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// isVisionModel 判断是否视觉模型：码道的对话模型不带图片，视觉版单独列出且
// 打进来会 406，所以不广告。
func isVisionModel(id string) bool {
	base := normalizeModelID(id)
	return strings.Contains(strings.ToLower(base), "-vl")
}

// normalizeModelID 去掉日期后缀（-1017）用于查上下文长度表。
func normalizeModelID(id string) string {
	if i := strings.LastIndexByte(id, '-'); i > 0 && len(id)-i == 5 {
		if isDigits(id[i+1:]) {
			return id[:i]
		}
	}
	return id
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var contextWindows = map[string]int{
	"GLM-5.2":             202752,
	"glm-5.3-flash":       1048576,
	"deepseek-v4-flash":   1048576,
	"deepseek-v4-pro":     1048576,
	"deepseek-v4.1-flash": 1000000,
}

func contextWindowOf(id string) int {
	if n, ok := contextWindows[normalizeModelID(id)]; ok {
		return n
	}
	return 1048576
}
