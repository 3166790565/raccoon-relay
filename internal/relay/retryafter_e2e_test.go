package relay

import (
	"context"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 上游 429 带 Retry-After：客户端应拿到同一个等待秒数，
// 且这个号的「账号×模型」冷却要对齐上游，而不是本地固定的 3 分钟。
func TestRateLimitHonorsUpstreamRetryAfter(t *testing.T) {
	h := newHarness(t, []chatResult{{
		status:  429,
		errCode: 429,
		errMsg:  "限流",
		headers: map[string]string{"Retry-After": "600"},
	}})
	h.engine.SetRetryPolicy(false, 0)
	a := h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 429 {
		t.Fatalf("应回 429，实际 %d：%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "600" {
		t.Fatalf("对外应带 Retry-After: 600，实际 %q", got)
	}

	infos := h.cool.ModelCooldownsOf(a.ID)
	if len(infos) != 1 {
		t.Fatalf("应有一条模型冷却，实际 %d", len(infos))
	}
	remain := time.Until(time.UnixMilli(infos[0].Until))
	if remain < 9*time.Minute || remain > 11*time.Minute {
		t.Fatalf("模型冷却应对齐上游 10 分钟，实际 %v", remain)
	}
}

// 号池全在冷却时，请求应真实打到上游一次，把上游的错误（含 Retry-After）
// 原样交给调用方，而不是直接回「没有可用账号」。
func TestProbeWhenPoolExhausted(t *testing.T) {
	h := newHarness(t, []chatResult{{
		status:  429,
		errCode: 429,
		errMsg:  "全部限流",
		headers: map[string]string{"Retry-After": "120"},
	}})
	a := h.addAccount(t, "pu-1", 100)
	// 手动把它放进冷却，池子里暂时没有可选的号
	until := time.Now().Add(time.Minute).UnixMilli()
	status := store.StatusCooling
	_ = h.st.UpdateAccount(context.Background(), a.ID, store.AccountPatch{Status: &status, CoolUntil: &until})

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 429 {
		t.Fatalf("探测应打上游并回 429，实际 %d：%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "120" {
		t.Fatalf("应透出上游的 Retry-After，实际 %q", got)
	}
	h.up.mu.Lock()
	hits := h.up.chatHits
	h.up.mu.Unlock()
	if hits != 1 {
		t.Fatalf("应恰好探测一次，实际 %d", hits)
	}
}
