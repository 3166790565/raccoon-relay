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
