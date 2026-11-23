package relay

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// 号池里剩下的健康号正被别的请求占着（在途租约）时，402 换号不能立刻放弃：
// 等它空出来再试，而不是把上一个号的上游错误原样丢给调用方。
func TestFailoverWaitsForBusyAccount(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 400, errBody: `{"error":{"code":1000007,"message":"积分余额不足，请充值后重试"}}`},
		{status: 200, frames: sseFrames("换号成功"), usage: usageFrame},
	})
	h.engine.Pool.SetLeaseConfig(pool.LeaseConfig{Enabled: true, Concurrency: 1, TTL: 5 * time.Minute})
	h.engine.SetRetryPolicy(false, 3)

	h.addAccount(t, "pu-poor", 1)
	rich := h.addAccount(t, "pu-rich", 9000)

	// 模拟健康号正在处理一个长回答：占住它的租约，400ms 后释放。
	// 这段时间短于 leaseWaitCap，请求应当等它，而不是立刻失败。
	grant, ok := h.engine.Pool.AcquireLease(context.Background(), rich)
	if !ok {
		t.Fatal("占租约失败")
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		h.engine.Pool.ReleaseAccountLease(context.Background(), rich, grant)
	}()

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("健康号空出来后应换号成功，实际 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "换号成功") {
		t.Fatalf("内容不对: %s", w.Body.String())
	}

	accounts, _ := h.st.ListAccounts(context.Background())
	for _, a := range accounts {
		if a.Name == "pu-poor" && a.Status != store.StatusExhausted {
			t.Fatalf("402 的号应记积分耗尽，实际 %s", a.Status)
		}
		if a.Name == "pu-rich" && a.Status != store.StatusOnline {
			t.Fatalf("健康号不该被改动，实际 %s", a.Status)
		}
	}
}

// 单号池、并发数 2：两个请求同时进来，第二个必须等到槽位空出来，而不是
// 直接 502。曾经的 bug 是「抢槽失败就把号写进 excluded」，导致号池被误判为空。
func TestSecondRequestWaitsForLeaseSlot(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 200, frames: sseFrames("first"), usage: usageFrame},
		{status: 200, frames: sseFrames("second"), usage: usageFrame},
	})
	h.engine.Pool.SetLeaseConfig(pool.LeaseConfig{Enabled: true, Concurrency: 2, TTL: 5 * time.Minute})
	h.engine.SetRetryPolicy(false, 3)
	acc := h.addAccount(t, "pu-only", 9000)

	// 占掉两个槽位中的一个：另一个请求还在跑长回答。
	grant, ok := h.engine.Pool.AcquireLease(context.Background(), acc)
	if !ok {
		t.Fatal("占租约失败")
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		h.engine.Pool.ReleaseAccountLease(context.Background(), acc, grant)
	}()

	// 只有一个空槽位可用；请求应当成功（拿不到时等槽位，而不是判池空）。
	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("单号池有空槽时应成功，实际 %d：%s", w.Code, w.Body.String())
	}
}

// 单号池并发数为 1、槽位被占满时，第二个请求要等它空出来再打，
// 而不是立刻回 502「号池没有可用账号」。
func TestPoolFullWaitsInsteadOf502(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 200, frames: sseFrames("waited"), usage: usageFrame},
	})
	h.engine.Pool.SetLeaseConfig(pool.LeaseConfig{Enabled: true, Concurrency: 1, TTL: 5 * time.Minute})
	h.engine.SetRetryPolicy(false, 3)
	acc := h.addAccount(t, "pu-only", 9000)

	grant, ok := h.engine.Pool.AcquireLease(context.Background(), acc)
	if !ok {
		t.Fatal("占租约失败")
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		h.engine.Pool.ReleaseAccountLease(context.Background(), acc, grant)
	}()

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "waited") {
		t.Fatalf("槽位空出后应成功，实际 %d：%s", w.Code, w.Body.String())
	}
}

// 健康号只会在号池真的还有在线号时才等：全部号都已冷却/耗尽时不能白等，
// 要尽快把错误交给调用方。
func TestFailoverDoesNotWaitWhenPoolDrained(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 400, errBody: `{"error":{"code":1000007,"message":"积分余额不足，请充值后重试"}}`},
	})
	h.engine.SetRetryPolicy(false, 3)
	h.addAccount(t, "pu-poor", 1)

	start := time.Now()
	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 402 {
		t.Fatalf("号池耗尽的错误应原样回 402，实际 %d：%s", w.Code, w.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("号池耗尽时不该等待，实际用了 %v", elapsed)
	}
}

// 会话粘性把请求绑到一个已耗尽的号上时，换号不能因此被卡住：
// 失败分支会解绑，选号层本来就跳过非在线号。
func TestFailoverIgnoresAffinityToExhaustedAccount(t *testing.T) {
	h := newHarness(t, []chatResult{
		{status: 402, errCode: 402, errMsg: "积分不足"},
		{status: 200, frames: sseFrames("换号成功"), usage: usageFrame},
	})
	h.addAccount(t, "pu-rich", 9000)
	h.addAccount(t, "pu-poor", 1)

	body := `{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`
	// 第一轮：把会话绑到耗尽的那个号上（先让它在号池里排第一）。
	first := h.do(t, "POST", "/v1/chat/completions", body)
	if first.Code != 200 {
		t.Fatalf("首轮应成功，实际 %d：%s", first.Code, first.Body.String())
	}
	// 直接改绑定表，模拟「上次成功的是这个穷号」。
	poor, _ := h.st.GetByUID(context.Background(), "pu-poor")
	if err := h.st.BindSession(context.Background(), "conv-x", poor.ID, "", time.Hour.Milliseconds(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	h.engine.Pool.SetPrefer(poor.ID)

	second := h.do(t, "POST", "/v1/chat/completions", body)
	if second.Code != 200 || !strings.Contains(second.Body.String(), "换号成功") {
		t.Fatalf("绑定到耗尽号时仍应换号成功，实际 %d：%s", second.Code, second.Body.String())
	}
}
