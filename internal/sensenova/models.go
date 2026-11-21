package sensenova

// 内置模型快照：来自平台自己的 /v1/models（context_length / max_output_length）。
// 只在实时清单拉不到时兜底，保证没登录的部署也有可选项。
type Model struct {
	ID            string
	Name          string
	ContextLength int
	MaxOutput     int
	SupportsImage bool
}

// FallbackModels 是日日新当前的模型阵容。数字取自平台接口，不是营销页。
var FallbackModels = []Model{
	{ID: "sensenova-6.8-flash-lite", Name: "SenseNova 6.8 Flash Lite", ContextLength: 262144, MaxOutput: 65536, SupportsImage: true},
	{ID: "deepseek-flash", Name: "DeepSeek Flash", ContextLength: 1048576, MaxOutput: 393216},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", ContextLength: 1048576, MaxOutput: 393216},
	{ID: "glm-5.2", Name: "GLM 5.2", ContextLength: 1048576, MaxOutput: 131072},
	{ID: "kimi-k3", Name: "Kimi K3", ContextLength: 1048576, MaxOutput: 65536},
}

// ModelByID 在内置快照里查一个模型。
func ModelByID(id string) (Model, bool) {
	for _, m := range FallbackModels {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// IsKnownModel 判断一个模型 id 是否属于日日新。用于把日日新模型与小浣熊
// 模型分开路由：两边的模型名不重叠（日日新是 deepseek-flash / glm-5.2 这类，
// 小浣熊是 raccoon-chat-ml-* / sn-sensenova-* 这类）。
//
// 注意：本函数不 import relay，relay 的另一侧同样只依赖本包不带 relay 的
// 子功能即可（relay 不调用本函数，由 admin 决定展示名单）。
func IsKnownModel(id string) bool {
	if _, ok := ModelByID(id); ok {
		return true
	}
	// 未知但形似日日新的模型（客户端可能用平台上新加的名字）：
	// 只要不是小浣熊前缀就归日日新。这个判断只在「按模型名分流」时用，
	// 选号时还会结合账号自身能力兜底。
	return !looksLikeRaccoon(id)
}

func looksLikeRaccoon(id string) bool {
	for _, prefix := range []string{"raccoon-", "sn-"} {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
