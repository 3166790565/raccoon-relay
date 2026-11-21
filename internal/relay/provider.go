package relay

import (
	"strings"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// providerForModel 按模型名决定请求该走哪个上游。放在 relay 内部（不 import
// sensenova，避免 relay ↔ sensenova 成环），规则与 sensenova.FallbackModels 保持同步。
//
// 两台上游的模型名互不重叠：日日新是 deepseek-flash / glm-5.2 / kimi-k3 /
// sensenova-6.8-flash-lite，小浣熊是 raccoon-chat-ml-* 与 sn-sensenova-*
// （sn- 前缀在小浣熊侧表示它转售的 sensenova 模型，和日日新的原生 id 不是一回事）。
//
// 默认模型（raccoon-chat-ml-5-5）走小浣熊；只有明确属于日日新的模型才走日日新，
// 其余（含未知/自定义模型名）一律按小浣熊处理，保持老客户端与测试兼容。
func providerForModel(model string) string {
	if isSenseNovaModel(model) {
		return store.ProviderSenseNova
	}
	return store.ProviderRaccoon
}

// sensenovaModels 是日日新的模型快照，数字取自平台 /v1/models。
// 与 internal/sensenova/models.go 的 FallbackModels 对齐；relay 不 import
// sensenova（会成环），必要处保留一份供 /v1/models 与路由使用。
var sensenovaModels = []raccoon.Model{
	{ID: "sensenova-6.8-flash-lite", Name: "SenseNova 6.8 Flash Lite", ContextLength: 262144, MaxOutputTokens: 65536, SupportsImages: true},
	{ID: "deepseek-flash", Name: "DeepSeek Flash", ContextLength: 1048576, MaxOutputTokens: 393216},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", ContextLength: 1048576, MaxOutputTokens: 393216},
	{ID: "glm-5.2", Name: "GLM 5.2", ContextLength: 1048576, MaxOutputTokens: 131072},
	{ID: "kimi-k3", Name: "Kimi K3", ContextLength: 1048576, MaxOutputTokens: 65536},
}

// snModelIDs 由模型快照派生，外加几个历史 id。
var snModelIDs = func() map[string]bool {
	m := map[string]bool{"sensenova-6.7-flash-lite": true, "deepseek-v4-flash": true}
	for _, x := range sensenovaModels {
		m[x.ID] = true
	}
	return m
}()

func isSenseNovaModel(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	// 只认已知的日日新模型。未知模型仍按小浣熊处理，保持老客户端与
	// 自定义模型名（例如测试里的 sn-test-model）不会突然被路由到另一台上游。
	return snModelIDs[id]
}
