package relay

import (
	"net/http"
	"testing"
)

// 同一段对话的两轮请求必须带同一个 X-RACCOON-Session-ID，
// 否则上游看到的是两条互不相干的请求（会话归并、上下文缓存全失效）。
func TestSessionHeaderStableAcrossTurns(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 200, frames: sseFrames("a"), usage: usageFrame},
		{status: 200, frames: sseFrames("b"), usage: usageFrame},
	})
	h.addAccount(t, "pu-sess", 1000)

	req1 := `{"model":"sn-test-model","messages":[{"role":"user","content":"第一个问题"}]}`
	req2 := `{"model":"sn-test-model","messages":[{"role":"user","content":"第一个问题"},` +
		`{"role":"assistant","content":"答"},{"role":"user","content":"第二个问题"}]}`
	for _, body := range []string{req1, req2} {
		r, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		_ = r
		if w := h.do(t, "POST", "/v1/chat/completions", body); w.Code != 200 {
			t.Fatalf("请求失败：%d %s", w.Code, w.Body.String())
		}
	}

	h.up.mu.Lock()
	defer h.up.mu.Unlock()
	if len(h.up.chatHeaders) != 2 {
		t.Fatalf("应记录两次对话请求头，实际 %d", len(h.up.chatHeaders))
	}
	s1 := h.up.chatHeaders[0].Get("X-RACCOON-Session-ID")
	s2 := h.up.chatHeaders[1].Get("X-RACCOON-Session-ID")
	if s1 == "" || s2 == "" {
		t.Fatal("对话请求必须带 X-RACCOON-Session-ID")
	}
	if s1 != s2 {
		t.Fatalf("同一段对话的两轮应共用 session：%q vs %q", s1, s2)
	}
	t1 := h.up.chatHeaders[0].Get("X-RACCOON-Turn-ID")
	t2 := h.up.chatHeaders[1].Get("X-RACCOON-Turn-ID")
	if t1 == "" || t2 == "" {
		t.Fatal("对话请求必须带 X-RACCOON-Turn-ID")
	}
	if t1 == t2 {
		t.Fatal("用户发了新消息，应形成新的一轮")
	}
}

// 客户端给了显式会话头时，同一会话的多轮必须共用同一个 session，
// 而不是按「首条消息」重新派生。
func TestSessionHeaderUsesExplicitKey(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 200, frames: sseFrames("a")},
		{status: 200, frames: sseFrames("b")},
	})
	h.addAccount(t, "pu-sess2", 1000)

	bodies := []string{
		`{"model":"sn-test-model","messages":[{"role":"user","content":"甲"}]}`,
		`{"model":"sn-test-model","messages":[{"role":"user","content":"乙"}]}`,
	}
	for _, body := range bodies {
		if w := h.doHeader(t, "X-Session-Id", "conv-abc", body); w.Code != 200 {
			t.Fatalf("请求失败：%d %s", w.Code, w.Body.String())
		}
	}

	h.up.mu.Lock()
	defer h.up.mu.Unlock()
	s1 := h.up.chatHeaders[0].Get("X-RACCOON-Session-ID")
	s2 := h.up.chatHeaders[1].Get("X-RACCOON-Session-ID")
	if s1 == "" || s1 != s2 {
		t.Fatalf("显式会话头的多轮应共用 session：%q vs %q", s1, s2)
	}
}

// 工具续轮：最后一条 user 消息之后追加的 tool 结果不改变 Turn。
func TestTurnHeaderStableAcrossToolContinues(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 200, frames: sseFrames("a")},
		{status: 200, frames: sseFrames("b")},
	})
	h.addAccount(t, "pu-turn", 1000)

	first := `{"model":"sn-test-model","messages":[{"role":"user","content":"查天气"}]}`
	// 续轮：同一轮 + 助手工具调用 + 工具结果
	second := `{"model":"sn-test-model","messages":[{"role":"user","content":"查天气"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"w","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"晴"}]}`
	for _, body := range []string{first, second} {
		if w := h.do(t, "POST", "/v1/chat/completions", body); w.Code != 200 {
			t.Fatalf("请求失败：%d %s", w.Code, w.Body.String())
		}
	}

	h.up.mu.Lock()
	defer h.up.mu.Unlock()
	t1 := h.up.chatHeaders[0].Get("X-RACCOON-Turn-ID")
	t2 := h.up.chatHeaders[1].Get("X-RACCOON-Turn-ID")
	if t1 == "" || t1 != t2 {
		t.Fatalf("工具续轮应复用同一 turn：%q vs %q", t1, t2)
	}
}
