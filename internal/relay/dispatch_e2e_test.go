package relay

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/store"
)

func okFrames() []string {
	return []string{`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`}
}

// 失败一次后，该号进入退避，同一请求不会再选它（避免密集重打同一个坏号）。
func TestEngineBackoffAfterTransientFailure(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 502, errMsg: "upstream boom"}, // 第一次：a 失败
		{frames: okFrames()},                   // 第二次：b 成功
	})
	// 积分差让 a 必定先被选中（负载均衡平局时积分多者优先）。
	a := h.addAccount(t, "pu-a", 1000)
	b := h.addAccount(t, "pu-b", 100)
	h.engine.Pool.SetBackoffPolicy(pool.BackoffPolicy{Enabled: true, Base: time.Second, Max: time.Minute})

	cc := BuildChatContext("sn-test-model", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, DefaultModel, "")

	if _, err := h.engine.RunOpenAI(context.Background(), cc, newAggWriter("sn-test-model")); err != nil {
		t.Fatalf("退避后应能换到另一个号成功: %v", err)
	}

	if h.up.chatHits != 2 {
		t.Fatalf("应恰好请求两次上游，实际 %d", h.up.chatHits)
	}
	failed, other := a, b
	until := h.engine.Pool.BackoffOf(context.Background(), failed.ID)
	if until <= time.Now().UnixMilli() {
		t.Fatalf("失败账号应进入退避窗口，实际 until=%d", until)
	}
	// 另一个号没失败，不该有退避。
	if got := h.engine.Pool.BackoffOf(context.Background(), other.ID); got != 0 {
		t.Fatalf("成功账号不该有退避，实际 %d", got)
	}
}

// 成功调用后清空退避链。
func TestEngineClearsBackoffOnSuccess(t *testing.T) {
	h := newHarness(t, []chatResult{{frames: okFrames()}})
	a := h.addAccount(t, "pu-a", 100)
	ctx := context.Background()

	// 先人为打一条「刚过期」的退避：它不会挡住选号，但成功后必须被清掉。
	// 直接打一条仍在生效的退避会把这个唯一的号挡在候选集外，测不到「成功后清退避」。
	h.engine.Pool.SetBackoffPolicy(pool.BackoffPolicy{Enabled: true, Base: time.Second, Max: time.Minute})
	_ = h.st.UpsertBackoff(ctx, store.AccountBackoff{
		AccountID: a.ID, Until: time.Now().UnixMilli() - 1, Reason: "旧失败", Hits: 3,
	})
	if h.engine.Pool.BackoffOf(ctx, a.ID) == 0 {
		t.Fatal("前置条件不成立")
	}

	cc := BuildChatContext("sn-test-model", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, DefaultModel, "")
	if _, err := h.engine.RunOpenAI(ctx, cc, newAggWriter("sn-test-model")); err != nil {
		t.Fatal(err)
	}
	if got := h.engine.Pool.BackoffOf(ctx, a.ID); got != 0 {
		t.Fatalf("成功后应清空退避，实际 %d", got)
	}
}

// 会话粘性：同一会话第二次请求优先回到上次成功的号。
func TestEngineSessionAffinityReusesAccount(t *testing.T) {
	h := newHarness(t, []chatResult{{frames: okFrames()}, {frames: okFrames()}})
	a := h.addAccount(t, "pu-a", 100)
	b := h.addAccount(t, "pu-b", 1000) // 积分更多，正常策略会优先选它
	h.engine.SetAffinityPolicy(true, time.Hour)

	ctx := context.Background()
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	cc1 := BuildChatContext("sn-test-model", body, DefaultModel, "conv-1")
	res1, err := h.engine.RunOpenAI(ctx, cc1, newAggWriter("sn-test-model"))
	if err != nil {
		t.Fatal(err)
	}

	// 第二轮：把第一轮用过的号压成「最不该被选」（积分清零 + 标记最久未用），
	// 只有粘性生效才会回到它。
	other := a
	if res1.AccountID == a.ID {
		other = b
	}
	zero := int64(0)
	_ = h.st.UpdateAccount(ctx, other.ID, store.AccountPatch{Credits: &zero})

	cc2 := BuildChatContext("sn-test-model", body, DefaultModel, "conv-1")
	res2, err := h.engine.RunOpenAI(ctx, cc2, newAggWriter("sn-test-model"))
	if err != nil {
		t.Fatal(err)
	}
	if res2.AccountID != res1.AccountID {
		t.Fatalf("同一会话应复用账号 %s，实际换成了 %s", res1.AccountID, res2.AccountID)
	}
}

// 绑定账号失败时立即解绑，下一轮允许换号（不会一直撞同一个号）。
func TestEngineAffinityUnbindsOnFailure(t *testing.T) {
	h := newHarness(t, []chatResult{
		{frames: okFrames()},          // 第一轮成功，绑定到某个号
		{status: 502, errMsg: "boom"}, // 第二轮：绑定的号失败
		{frames: okFrames()},          // 第二轮换号后成功
	})
	h.addAccount(t, "pu-a", 100)
	h.addAccount(t, "pu-b", 100)
	h.engine.SetAffinityPolicy(true, time.Hour)

	ctx := context.Background()
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	res1, err := h.engine.RunOpenAI(ctx, BuildChatContext("sn-test-model", body, DefaultModel, "conv-1"), newAggWriter("sn-test-model"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.st.BoundAccount(ctx, "conv-1", time.Now().UnixMilli()); !ok {
		t.Fatal("成功后应建立会话绑定")
	}

	// 第二轮先失败一次再成功：最终绑定应指向最终成功的号，而不是失败的那个。
	res2, err := h.engine.RunOpenAI(ctx, BuildChatContext("sn-test-model", body, DefaultModel, "conv-1"), newAggWriter("sn-test-model"))
	if err != nil {
		t.Fatal(err)
	}
	if res2.AccountID == res1.AccountID {
		t.Fatalf("绑定号失败后应换到别的号，实际仍是 %s", res2.AccountID)
	}
	bound, ok := h.st.BoundAccount(ctx, "conv-1", time.Now().UnixMilli())
	if !ok || bound != res2.AccountID {
		t.Fatalf("绑定应更新为成功账号 %s，实际 %s", res2.AccountID, bound)
	}
}

// 换号间隔应真正生效：失败后不会立刻发起下一次上游请求。
func TestEngineRetryDelay(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 502, errMsg: "boom"}, {frames: okFrames()}})
	h.addAccount(t, "pu-a", 100)
	h.addAccount(t, "pu-b", 100)
	h.engine.SetRetryDelay(120 * time.Millisecond)

	start := time.Now()
	cc := BuildChatContext("sn-test-model", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, DefaultModel, "")
	if _, err := h.engine.RunOpenAI(context.Background(), cc, newAggWriter("sn-test-model")); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Fatalf("换号前应等待至少 120ms，实际 %s", elapsed)
	}
}

// 会话标识解析：头优先于请求体；取不到时返回空串（粘性自动关闭）。
func TestSessionKeyOf(t *testing.T) {
	body := map[string]any{"session_id": "body-id", "conversation_id": "conv-body"}

	req, _ := http.NewRequest("POST", "/", nil)
	if got := SessionKeyOf(req, body); got != "body-id" {
		t.Fatalf("应取请求体 session_id，实际 %q", got)
	}
	req.Header.Set("X-Session-Id", "hdr-id")
	if got := SessionKeyOf(req, body); got != "hdr-id" {
		t.Fatalf("头应优先于请求体，实际 %q", got)
	}
	req.Header.Set("X-Session-Id", "")
	req.Header.Set("X-Conversation-Id", "hdr-conv")
	if got := SessionKeyOf(req, map[string]any{}); got != "hdr-conv" {
		t.Fatalf("应回落到 X-Conversation-Id，实际 %q", got)
	}
	if got := SessionKeyOf(req, map[string]any{}); got != "hdr-conv" {
		t.Fatalf("不应受请求体影响，实际 %q", got)
	}

	empty, _ := http.NewRequest("POST", "/", nil)
	if got := SessionKeyOf(empty, map[string]any{}); got != "" {
		t.Fatalf("取不到时应返回空串，实际 %q", got)
	}
}

// prompt_cache_key 是 OpenAI SDK 的缓存路由字段：它必须能当会话键用，
// 否则客户端想命中前缀缓存，我们却每轮换号。
func TestSessionKeyOfPromptCacheKey(t *testing.T) {
	req, _ := http.NewRequest("POST", "/", nil)
	body := map[string]any{"prompt_cache_key": "tenant-abc"}
	if got := SessionKeyOf(req, body); got != "tenant-abc" {
		t.Fatalf("应把 prompt_cache_key 当会话键，实际 %q", got)
	}
	// 显式会话键优先于它。
	if got := SessionKeyOf(req, map[string]any{"session_id": "s1", "prompt_cache_key": "p1"}); got != "s1" {
		t.Fatalf("显式会话键应优先，实际 %q", got)
	}
}

// 无会话键时的兜底粘性：按首条 user 消息派生，且开关关掉后不再兜底。
func TestStickyKeyFallback(t *testing.T) {
	e := &Engine{}
	e.SetAffinityFallback(true)
	msgs := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "同一个问题"},
		map[string]any{"role": "assistant", "content": "答"},
		map[string]any{"role": "user", "content": "追问"},
	}
	k1 := e.stickyKey("", msgs)
	k2 := e.stickyKey("", msgs)
	if k1 == "" || k1 != k2 {
		t.Fatalf("同样的首条消息应派生出稳定兜底键，k1=%q k2=%q", k1, k2)
	}
	if k1 == "同一个问题" {
		t.Fatal("兜底键应带前缀，避免与客户端显式会话键撞车")
	}
	// 首条不同 → 不同键
	k3 := e.stickyKey("", []any{map[string]any{"role": "user", "content": "另一个问题"}})
	if k3 == k1 {
		t.Fatal("不同首条消息不应共用同一个兜底键")
	}
	// 显式键优先
	if got := e.stickyKey("explicit", msgs); got != "explicit" {
		t.Fatalf("显式会话键应原样使用，实际 %q", got)
	}
	// 关掉兜底 → 无键时空
	e.SetAffinityFallback(false)
	if got := e.stickyKey("", msgs); got != "" {
		t.Fatalf("兜底关闭时应返回空串，实际 %q", got)
	}
	// 完全没有 user 消息时也不兜底
	e.SetAffinityFallback(true)
	if got := e.stickyKey("", []any{map[string]any{"role": "system", "content": "s"}}); got != "" {
		t.Fatalf("没有 user 消息时不应兜底，实际 %q", got)
	}
}
