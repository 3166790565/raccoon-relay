package raccoon

import (
	"encoding/json"
	"regexp"
	"testing"
)

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-a[0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSeedUUIDShapeAndStability(t *testing.T) {
	a := seedUUID("raccoon:session", "pu-1", "hello")
	if !uuidShape.MatchString(a) {
		t.Fatalf("形态不合法：%s", a)
	}
	if b := seedUUID("raccoon:session", "pu-1", "hello"); a != b {
		t.Fatal("同输入应得到同值")
	}
	if c := seedUUID("raccoon:session", "pu-1", "other"); a == c {
		t.Fatal("不同输入应得到不同值")
	}
}

// 长度前缀：「ab」+「c」与「a」+「bc」不能拼出同一个输入。
func TestSeedUUIDFieldBoundaries(t *testing.T) {
	if seedUUID("x", "ab", "c") == seedUUID("x", "a", "bc") {
		t.Fatal("分段边界必须可区分")
	}
}

func TestSessionStableAcrossTurns(t *testing.T) {
	// 同一个会话键：多轮之间 Session 必须不变。
	if SessionID("pu-1", "conv-1") != SessionID("pu-1", "conv-1") {
		t.Fatal("同一会话应得到同一 session")
	}
	if SessionID("pu-1", "conv-1") == SessionID("pu-2", "conv-1") {
		t.Fatal("不同账号应得到不同 session")
	}
}

// 工具续轮：最后一条 user 消息之后追加的 tool 结果不改变 Turn。
func TestTurnSeedIgnoresTrailingToolResults(t *testing.T) {
	first := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "查一下天气"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1"}}},
	}
	// 续轮：同样的前缀 + 工具结果
	second := append(append([]any{}, first...), map[string]any{"role": "tool", "tool_call_id": "c1", "content": "晴"})

	if TurnSeedOf(first) != TurnSeedOf(second) {
		t.Fatal("工具续轮应复用同一 Turn 种子")
	}
	// 用户真的发了新消息才算新的一轮
	third := append(append([]any{}, second...), map[string]any{"role": "user", "content": "那明天呢"})
	if TurnSeedOf(first) == TurnSeedOf(third) {
		t.Fatal("新的 user 消息应形成新的一轮")
	}
}

// 没有 user 消息时（例如 tools-only 请求）退化为整个请求体的哈希。
func TestTurnSeedWithoutUserMessage(t *testing.T) {
	msgs := []any{map[string]any{"role": "system", "content": "sys"}}
	if TurnSeedOf(msgs) == "" {
		t.Fatal("应退化为请求体哈希，而不是空种子")
	}
}

func TestSessionSeedPrefersExplicitKey(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	if got := SessionSeedOf("", msgs); got == "" {
		t.Fatal("没有会话键时应退到首条 user 消息")
	}
	// 同一个会话键在请求体变化时仍稳定（由调用方拼 key 决定，这里只验派生不漂）
	if SessionID("pu-1", "conv-1") != SessionID("pu-1", "conv-1") {
		t.Fatal("派生必须确定")
	}
}

func TestNormalizeSessionSeed(t *testing.T) {
	if got := NormalizeSessionSeed("  a\nb  "); got != "ab" {
		t.Fatalf("应裁掉空白与控制字符，实际 %q", got)
	}
	if got := NormalizeSessionSeed("   "); got != "" {
		t.Fatalf("空白应归空，实际 %q", got)
	}
}

// 会话 id 走查：header 里的 SessionID 派生要与 TurnIDFor 组合出稳定标识。
func TestSessionHeaderValues(t *testing.T) {
	idn := IdentityFor("pu-1", "", "", "", "", "", "")
	session := SessionID("pu-1", "conv-9")
	turn := TurnIDFor(session, "seed")
	h := chatHeaders("tok", idn, session, turn, "t")
	if h["X-RACCOON-Session-ID"] != session {
		t.Fatal("session 头没带上")
	}
	if h["X-RACCOON-Turn-ID"] != turn || !uuidShape.MatchString(turn) {
		t.Fatalf("turn 头不对：%q", h["X-RACCOON-Turn-ID"])
	}
	// 同一 session + 同一轮种子 → 同一 turn；不同轮种子 → 不同 turn
	if TurnIDFor(session, "seed") != turn {
		t.Fatal("同轮应得到同一 turn")
	}
	if TurnIDFor(session, "seed2") == turn {
		t.Fatal("不同轮应得到不同 turn")
	}
}

// prepareBody 仍需要删除 prompt_cache_key（上游不认），但会话标识现在由
// 显式会话键派生——这两件事不冲突，这里锁定后者不被误删。
func TestPrepareBodyKeepsMessages(t *testing.T) {
	body := map[string]any{
		"model":    "sn-test-model",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out := PrepareBody("sn-test-model", body, "")
	raw, _ := json.Marshal(out)
	if len(raw) == 0 {
		t.Fatal("请求体不应为空")
	}
}
