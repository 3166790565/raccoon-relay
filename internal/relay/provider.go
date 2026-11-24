package relay

import (
	"context"
	"strings"
	"sync"
	"time"

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

// SNModelSource 是日日新实时模型清单的来源，由装配层注入 sensenova.Upstream。
// relay 不 import sensenova（会成环），只认这个最小接口。
type SNModelSource interface {
	// ModelsFor 返回该账号可见的实时模型清单；拉不到时返回 nil。
	ModelsFor(ctx context.Context, acc *store.Account) []raccoon.Model
}

// sensenovaModels 是日日新的模型快照兜底：实时清单拉不到时才用
// （没有可用账号、上游故障、首次启动还没拿到登录票）。
// 与 internal/sensenova/models.go 的 FallbackModels 对齐；relay 不 import
// sensenova（会成环），必要处保留一份供 /v1/models 与路由使用。
var sensenovaModels = []raccoon.Model{
	{ID: "sensenova-6.8-flash-lite", Name: "SenseNova 6.8 Flash Lite", ContextLength: 262144, MaxOutputTokens: 65536, SupportsImages: true},
	{ID: "sensenova-6.8-flash", Name: "SenseNova 6.8 Flash", ContextLength: 262144, MaxOutputTokens: 65536, SupportsImages: true},
	{ID: "deepseek-flash", Name: "DeepSeek Flash", ContextLength: 1048576, MaxOutputTokens: 65536},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", ContextLength: 1048576, MaxOutputTokens: 65536},
	{ID: "glm-5.2", Name: "GLM 5.2", ContextLength: 1048576, MaxOutputTokens: 131072},
	{ID: "glm-5.3", Name: "GLM 5.3", ContextLength: 1048576, MaxOutputTokens: 131072},
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
	if snModelIDs[id] {
		return true
	}
	// 上游新上线的模型靠实时清单补充；清单里没有的未知模型仍按小浣熊处理，
	// 保持老客户端与自定义模型名（例如测试里的 sn-test-model）不会突然被路由走。
	return liveSNModels.contains(id)
}

// liveSNModels 是最近一次从上游拉到的日日新模型 id 集合。
// 只做「路由判断」的补充，不替代快照：清单过期或从没拉到过时，
// snModelIDs 仍覆盖已知阵容。
var liveSNModels = &liveSNModelSet{}

type liveSNModelSet struct {
	mu   sync.RWMutex
	ids  map[string]bool
	at   time.Time
	seen bool
}

func (s *liveSNModelSet) replace(ids []string) {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		if v := strings.ToLower(strings.TrimSpace(id)); v != "" {
			m[v] = true
		}
	}
	if len(m) == 0 {
		return
	}
	s.mu.Lock()
	s.ids, s.at, s.seen = m, time.Now(), true
	s.mu.Unlock()
}

// ModelDisabledError 是请求了一个被模型管理停用的模型。
type ModelDisabledError struct{ Model string }

func (e *ModelDisabledError) Error() string {
	return "模型 " + e.Model + " 已被停用"
}

func (s *liveSNModelSet) contains(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seen && s.ids[id]
}
