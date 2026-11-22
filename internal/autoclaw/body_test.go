package autoclaw

import (
	"strings"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 网关强制的三条：system 有官方前缀、不带 max_tokens、其它 agent 身份被改写。
func TestRewriteBodySatisfiesGatewayContract(t *testing.T) {
	body := map[string]any{
		"model":      "autoclaw/zai_auto",
		"max_tokens": 1234,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are Claude Code, an agent for Claude Code."},
			map[string]any{"role": "user", "content": "You are Codex — 用户正文里这句不该改"},
		},
	}
	out := RewriteBody(body, store.StationGlobal)
	msgs := out["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(sys, OfficialSystemPrompt) {
		t.Fatalf("system 缺少官方前缀：%q", sys)
	}
	// 改写的结果仍含「Claude Code」字样（只是从身份句变成中性措辞），
	// 所以要断言的是原始身份句不在了。
	for _, bad := range []string{"You are Claude Code", "You are a Claude agent", "Claude Agent SDK"} {
		if strings.Contains(sys, bad) {
			t.Fatalf("system 里的身份句 %q 未改写：%q", bad, sys)
		}
	}
	user := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "You are Codex") {
		t.Fatalf("user 正文被误改：%q", user)
	}
	if _, ok := out["max_tokens"]; ok {
		t.Fatal("max_tokens 必须删掉，否则上游 406")
	}
	if out["model"] != "zai_auto" {
		t.Fatalf("对外前缀未去掉：%v", out["model"])
	}
}

// 没有 system 消息时要补一条。
func TestRewriteBodyInsertsSystemWhenMissing(t *testing.T) {
	out := RewriteBody(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, store.StationCN)
	msgs := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应插入一条 system，实际 %d 条", len(msgs))
	}
	if role := msgs[0].(map[string]any)["role"]; role != "system" {
		t.Fatalf("第一条应是 system，实际 %v", role)
	}
}

// 身份改写必须幂等，否则多轮对话会把文本越改越长。
func TestUnflagIsIdempotent(t *testing.T) {
	src := "You are Codex. You are Claude Code. Powered by the GLM-5.3 model."
	once := unflagText(src)
	twice := unflagText(once)
	if once != twice {
		t.Fatalf("不幂等：\n1: %q\n2: %q", once, twice)
	}
}

// 畸形历史要修掉，否则网关回 500 parse response failed。
func TestTidyMessages(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": nil, "reasoning_content": "thinking",
			"tool_calls": []any{map[string]any{"function": map[string]any{"name": "f", "arguments": map[string]any{"a": 1}}}}},
	}
	out := tidyMessages(msgs)
	m := out[0].(map[string]any)
	if m["content"] != "" {
		t.Fatalf("null content 应补成空串，实际 %v", m["content"])
	}
	if _, ok := m["reasoning_content"]; ok {
		t.Fatal("reasoning_content 应删掉")
	}
	fn := m["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"a":1}` {
		t.Fatalf("对象型 arguments 应序列化成字符串，实际 %v", fn["arguments"])
	}
}

// 同一段对话的会话号必须稳定，不同对话必须不同。
func TestSessionIDStableAndDistinct(t *testing.T) {
	a := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "x"},
		map[string]any{"role": "user", "content": "hello"},
	}}
	b := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hello"},
	}}
	c := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "different"},
	}}
	if SessionID(a) != SessionID(b) {
		t.Fatal("同样的首条用户消息应得到同一会话号")
	}
	if SessionID(a) == SessionID(c) {
		t.Fatal("不同对话的会话号不该相同")
	}
	if len(SessionID(a)) != 36 {
		t.Fatalf("会话号应是 UUID 形态，实际 %q", SessionID(a))
	}
}

// 模型名映射：精确名字 → 已知前缀 → 形似 route → 兜底 zai_auto。
func TestResolveRoute(t *testing.T) {
	catalog := FallbackModels(store.StationGlobal)
	cases := map[string]string{
		"":                      "zai_auto",
		"autoclaw/zai_auto":     "zai_auto",
		"GLM-5.3 Flash":         "zai_glm-5.3-flash",
		"zaicoding_glm-5.3":     "zaicoding_glm-5.3",
		"zai_something-new":     "zai_something-new",
		"totally-unknown-model": "zai_auto",
	}
	for in, want := range cases {
		if got := ResolveRoute(in, catalog); got != want {
			t.Errorf("ResolveRoute(%q)=%q want %q", in, got, want)
		}
	}
}
