package relay

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 端到端：客户端中途取消，流水必须落 499，且不污染失败数。
//
// 这条链路串起三处修复：ChatStream 保留 context.Canceled（否则 classify 落到
// 502）、publicError 认得出客户端断开（否则流水记 502）、store 的口径把 499
// 排除在失败之外（否则面板仍然显示成失败）。
func TestClientCancelRecords499Usage(t *testing.T) {
	h := newHarness(t, []chatResult{{
		frames:          []string{`{"choices":[{"delta":{"content":"半截"}}]}`},
		hangAfterFrames: true,
	}})
	h.addAccount(t, "pu-1", 100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &cancelAfterFirstDelta{
		inner:  newSSEWriter(httptest.NewRecorder(), "sn-test-model"),
		cancel: cancel,
	}
	cc := BuildChatContext("sn-test-model", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, DefaultModel, "")

	start := time.Now()
	res, err := h.engine.RunOpenAI(ctx, cc, sink)
	// 与线上 handler 完全相同的记账调用。
	h.srv.logUsage(context.Background(), res, err, start, "openai", nil)
	h.st.FlushUsage()

	_, _, failed, qerr := h.st.QueryUsage(context.Background(), store.UsageFilter{})
	if qerr != nil {
		t.Fatal(qerr)
	}
	if failed != 0 {
		t.Fatalf("客户端取消不该计入失败数，实际 %d", failed)
	}

	canceled, _, _, qerr := h.st.QueryUsage(context.Background(), store.UsageFilter{Status: "canceled"})
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(canceled) != 1 || canceled[0].Status != store.UsageStatusCanceled {
		t.Fatalf("应有 1 条 499 流水，实际 %+v", canceled)
	}
	if canceled[0].AccountID == "" {
		t.Fatal("流水应保留账号，事后才知道是哪个号在服务")
	}
}
