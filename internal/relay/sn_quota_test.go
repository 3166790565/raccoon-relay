package relay

import (
	"context"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// fakeSN 是 SNUpstream 的桩：推理请求固定返回给定的错误。
type fakeSN struct{ err error }

func (f fakeSN) OpenChat(context.Context, string, map[string]any) (*raccoon.ChatStream, error) {
	return nil, f.err
}

func (f fakeSN) EnsureToken(context.Context, *store.Account) (*SNCredential, error) {
	return nil, nil
}

func (f fakeSN) ProvisionKey(context.Context, *store.Account) (string, error) { return "", nil }

// fakeSNQuota 是 SNQuotaSource 的桩：固定返回一组窗口读数。
type fakeSNQuota struct {
	until   int64
	ok      bool
	credits int64
	stale   bool
	calls   int
}

func (f *fakeSNQuota) NextReset(_ context.Context, _ *store.Account) (int64, bool, int64, bool) {
	f.calls++
	return f.until, f.ok, f.credits, f.stale
}

// 日日新 402：冷却对齐上游窗口重置时刻，而不是「下一次签到」。
// 额度确实用尽时把读到的 0 落库，否则号到期放回在线还会被「0 分不出池」挡住。
func TestSenseNovaQuotaUsesWindowReset(t *testing.T) {
	h := newHarness(t, []chatResult{{
		status:  402,
		errBody: `{"error":{"code":"insufficient_quota","message":"额度已用尽"}}`,
	}})
	h.engine.SN = fakeSN{err: &raccoon.UpstreamError{
		Status: 402, Code: 1000007, Message: "额度已用尽",
	}}
	h.engine.SetRetryPolicy(false, 0)
	q := &fakeSNQuota{
		until: time.Now().Add(2 * time.Hour).UnixMilli(), ok: true,
		credits: 0, stale: true,
	}
	h.engine.SNQuota = q

	acc := &store.Account{
		UID: "sn-1", Name: "sn-1", Provider: store.ProviderSenseNova,
		Token: "sk-test", Status: store.StatusOnline, Credits: 12345,
		CreditsAt: time.Now().UnixMilli(), Enabled: true,
	}
	if err := h.st.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 402 {
		t.Fatalf("应回 402，实际 %d：%s", w.Code, w.Body.String())
	}
	if q.calls != 1 {
		t.Fatalf("应查一次额度窗口，实际 %d", q.calls)
	}

	got, _ := h.st.GetAccount(context.Background(), acc.ID)
	if got.Status != store.StatusExhausted {
		t.Fatalf("应为 exhausted，实际 %s", got.Status)
	}
	if got.CoolUntil != q.until {
		t.Fatalf("冷却应停在窗口重置 %d，实际 %d", q.until, got.CoolUntil)
	}
	if got.Credits != 0 {
		t.Fatalf("额度用尽应把 0 落库，实际 %d", got.Credits)
	}
}

// 上游没给窗口重置时刻：回退到通用路径（签到计划/默认时刻），不把号永久停掉，
// 也不覆盖旧余额。
func TestSenseNovaQuotaFallsBackWithoutReset(t *testing.T) {
	h := newHarness(t, []chatResult{{
		status:  402,
		errBody: `{"error":{"code":"insufficient_quota","message":"额度已用尽"}}`,
	}})
	h.engine.SN = fakeSN{err: &raccoon.UpstreamError{
		Status: 402, Code: 1000007, Message: "额度已用尽",
	}}
	h.engine.SetRetryPolicy(false, 0)
	h.engine.SNQuota = &fakeSNQuota{ok: false, credits: 0, stale: true}

	acc := &store.Account{
		UID: "sn-2", Name: "sn-2", Provider: store.ProviderSenseNova,
		Token: "sk-test", Status: store.StatusOnline, Credits: 999,
		CreditsAt: time.Now().UnixMilli(), Enabled: true,
	}
	if err := h.st.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}

	_ = h.do(t, "POST", "/v1/chat/completions",
		`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}`)

	got, _ := h.st.GetAccount(context.Background(), acc.ID)
	if got.Status != store.StatusExhausted || got.CoolUntil <= time.Now().UnixMilli() {
		t.Fatalf("回退也应停号且带未来到期时刻，实际 %s/%d", got.Status, got.CoolUntil)
	}
	if got.Credits != 999 {
		t.Fatalf("没有可信读数时不应覆盖旧余额，实际 %d", got.Credits)
	}
}

// 小浣熊账号不受影响：仍然走签到计划/默认时刻，不查日日新额度窗口。
func TestRaccoonQuotaDoesNotUseSNWindow(t *testing.T) {
	h := newHarness(t, []chatResult{{
		status: 402, errBody: `{"code":1000007,"message":"积分余额不足"}`,
	}})
	h.engine.SetRetryPolicy(false, 0)
	q := &fakeSNQuota{until: time.Now().Add(time.Hour).UnixMilli(), ok: true, stale: true}
	h.engine.SNQuota = q
	h.addAccount(t, "pu-1", 100)

	_ = h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)

	if q.calls != 0 {
		t.Fatalf("小浣熊不应调用日日新额度查询，实际 %d 次", q.calls)
	}
}

var _ = raccoon.UpstreamError{}
