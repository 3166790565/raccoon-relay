package relay

import (
	"strings"
	"sync"

	"github.com/3166790565/raccoon-relay/internal/sanitize"
)

// GatewayPolicy 是「发给上游的副本怎么改」的那一组开关：指纹脱敏、开发者
// 角色兼容，以及模型清单要不要用 models.dev 补齐能力位。
//
// 三者在面板「设置 → 网关」里热更新。数据面逐请求读一次快照，因此改完的
// 下一个请求立即生效，不重启进程。
//
// 为什么单独一个类型：它们都只在 relay 的出站路径上被读，且相互独立；
// 挂在 Server 上逐个字段加锁会让读写点散落。一份快照整体替换最不容易出错。
type GatewayPolicy struct {
	// SanitizeOn 指出站请求体是否按词表脱敏。
	SanitizeOn bool
	// SanitizeProviders 为空 = 应用到全部提供商（含之后新增的）。
	SanitizeProviders []string
	// SanitizeRules 是编译好的词表（nil = 不过滤）。
	SanitizeRules *sanitize.RuleSet
	// DeveloperRoleCompat 指出站前是否把 role:"developer" 归一为 "system"。
	DeveloperRoleCompat bool
	// ModelsDev 表示是否用 models.dev 补齐 /v1/models 的能力位。
	ModelsDev bool
	// MetaLookup 查一条 models.dev 元数据（nil = 未接入）。
	MetaLookup func(model string) *MetaRecord
}

// MetaRecord 是 models.dev 一条记录的只读视图（relay 侧的最小形状）。
type MetaRecord struct {
	Reasoning *bool
	ToolCall  *bool
	Images    *bool
	Video     *bool
	Context   *int64
	Output    *int64
	Levels    []string
}

// gatewayMu 保护当前策略快照。读多写极少（只在面板保存时整体替换）。
var (
	gatewayMu     sync.RWMutex
	gatewayPolicy = GatewayPolicy{}
)

// SetGatewayPolicy 整体替换出站策略（装配层与设置热更新调用）。
func SetGatewayPolicy(p GatewayPolicy) {
	gatewayMu.Lock()
	gatewayPolicy = p
	gatewayMu.Unlock()
}

// CurrentGatewayPolicy 返回当前策略快照（值拷贝，调用方随便用）。
func CurrentGatewayPolicy() GatewayPolicy {
	gatewayMu.RLock()
	defer gatewayMu.RUnlock()
	return gatewayPolicy
}

// SanitizeAppliesTo 判断脱敏是否作用于某个提供商。
func (p GatewayPolicy) SanitizeAppliesTo(provider string) bool {
	if !p.SanitizeOn || p.SanitizeRules.Empty() {
		return false
	}
	if len(p.SanitizeProviders) == 0 {
		return true
	}
	for _, id := range p.SanitizeProviders {
		if strings.EqualFold(id, provider) {
			return true
		}
	}
	return false
}

// normalizeDeveloperRole 把 messages 里 role:"developer" 的消息改写为
// "system"（开发者角色兼容）。
//
// 返回 false 表示一个字节都没动（开关关着 / 没有 messages / 里面没有
// developer 消息）—— 调用方继续沿用原请求体，零拷贝路径不被打扰。
// 只有预检到 developer 消息才复制整份请求体。
func normalizeDeveloperRole(body map[string]any) (map[string]any, bool) {
	if body == nil {
		return body, false
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return body, false
	}
	hits := 0
	for _, raw := range messages {
		if msg, ok := raw.(map[string]any); ok {
			if role, _ := msg["role"].(string); strings.EqualFold(role, "developer") {
				hits++
			}
		}
	}
	if hits == 0 {
		return body, false
	}
	next := make(map[string]any, len(body))
	for k, v := range body {
		next[k] = v
	}
	cloned := make([]any, len(messages))
	for i, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			cloned[i] = raw
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(role, "developer") {
			replaced := make(map[string]any, len(msg))
			for k, v := range msg {
				replaced[k] = v
			}
			replaced["role"] = "system"
			cloned[i] = replaced
			continue
		}
		cloned[i] = raw
	}
	next["messages"] = cloned
	return next, true
}

// NormalizeDeveloperRoleForTest 暴露给管理面的设置测试：验证「developer →
// system」的归一行为与零拷贝语义。生产路径只通过 applyGateway 调用。
func NormalizeDeveloperRoleForTest(body map[string]any) (map[string]any, bool) {
	return normalizeDeveloperRole(body)
}
