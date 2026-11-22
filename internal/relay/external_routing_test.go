package relay

import (
	"testing"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 带渠道前缀的模型必须路由到对应渠道；去掉前缀后不得误判。
func TestProviderForModelRoutesExternalChannels(t *testing.T) {
	cases := map[string]string{
		"autoclaw/zai_auto":            store.ProviderAutoClaw,
		"AutoClaw/zai_auto":            store.ProviderAutoClaw,
		"codearts/GLM-5.2":             store.ProviderCodeArts,
		"codearts/deepseek-v4.1-flash": store.ProviderCodeArts,
		// 无前缀的 glm-5.2 仍是日日新（外部渠道必须带前缀，否则名字会互相抢）。
		"glm-5.2":             store.ProviderSenseNova,
		"raccoon-chat-ml-5-5": store.ProviderRaccoon,
		"whatever-model":      store.ProviderRaccoon,
		// 前缀后面为空不算有效前缀。
		"autoclaw/": store.ProviderRaccoon,
	}
	for model, want := range cases {
		if got := providerForModel(model); got != want {
			t.Errorf("providerForModel(%q)=%q want %q", model, got, want)
		}
	}
}

func TestStripProviderPrefix(t *testing.T) {
	cases := map[string]string{
		"autoclaw/zai_auto":      "zai_auto",
		"codearts/GLM-5.2":       "GLM-5.2",
		"glm-5.2":                "glm-5.2",
		"sn-sensenova-6.8-flash": "sn-sensenova-6.8-flash",
		"autoclaw-cn/whatever":   "autoclaw-cn/whatever",
	}
	for in, want := range cases {
		if got := stripProviderPrefix(in); got != want {
			t.Errorf("stripProviderPrefix(%q)=%q want %q", in, got, want)
		}
	}
}
