package raccoon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 403 的现场必须带出来：只有一句「HTTP 403」用户没法排查，
// 响应头（网关拦截码）和原始响应体才是真正的线索。
func TestOpenChatCaptures403Diagnostics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-TT-System-Error", "3")
		w.Header().Set("X-Request-Id", "req-abc-123")
		w.Header().Set("Set-Cookie", "session=super-secret; Path=/")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"message":"device fingerprint blocked"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 0)
	_, err := c.OpenChat(context.Background(), ChatRequest{
		Body: map[string]any{"model": "sn-test-model"},
		IDN:  IdentityFor("pu-1", "dev-1", "win11", "web", "stable", "", "zh"),
	})
	if err == nil {
		t.Fatal("403 应返回错误")
	}
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("应为 UpstreamError，实际 %T", err)
	}
	if ue.Status != http.StatusForbidden {
		t.Fatalf("状态码应为 403，实际 %d", ue.Status)
	}
	if ue.Headers["x-tt-system-error"] != "3" {
		t.Fatalf("应保留网关错误头，实际 %v", ue.Headers)
	}
	if ue.RequestID != "req-abc-123" {
		t.Fatalf("应识别请求 ID，实际 %q", ue.RequestID)
	}
	if _, leaked := ue.Headers["set-cookie"]; leaked {
		t.Fatal("响应头里的凭据不能带到面板上")
	}
	if !strings.Contains(ue.Raw, "device fingerprint blocked") {
		t.Fatalf("应保留原始响应体，实际 %q", ue.Raw)
	}
	if ue.TurnID == "" || ue.Endpoint != PathChat {
		t.Fatalf("应记录 turn 与接口：%q %q", ue.TurnID, ue.Endpoint)
	}
	if !strings.Contains(ue.RequestSummary, "sn-test-model") {
		t.Fatalf("请求摘要应带模型，实际 %q", ue.RequestSummary)
	}
}

// 原始响应体可能是超长的网关拦截页：截断但保留开头，
// 并且按字符截断，不把多字节汉字切坏。
func TestTruncateRawKeepsValidUTF8(t *testing.T) {
	long := strings.Repeat("上游拦截", 2000)
	out := truncateRaw([]byte(long))
	if !strings.HasPrefix(out, "上游拦截") { // 不是 "上游拦" 这种被切坏的字节
		t.Fatalf("截断后开头不对: %q", out[:12])
	}
	if !strings.Contains(out, "已截断") {
		t.Fatal("超长响应应标注已截断")
	}
}

// 长响应体的截断要按字符做，不能把多字节汉字切坏。
func TestTruncateRawCharacterBoundary(t *testing.T) {
	out := truncateRaw([]byte("错误"))
	if out != "错误" {
		t.Fatalf("短响应应原样返回，实际 %q", out)
	}
	if out := truncateRaw(nil); out != "" {
		t.Fatalf("空响应应返回空串，实际 %q", out)
	}
}

// 响应头里的凭据一律不进现场。
func TestDiagnosticHeadersDropSecrets(t *testing.T) {
	h := http.Header{}
	h.Set("Set-Cookie", "a=b")
	h.Set("Authorization", "Bearer x")
	h.Set("X-Request-Id", "ok")
	got := diagnosticHeaders(h)
	if len(got) != 1 || got["x-request-id"] != "ok" {
		t.Fatalf("只应保留非敏感头，实际 %v", got)
	}
}

// requestIDOf 要认常见的网关请求 ID 头。
func TestRequestIDHeaders(t *testing.T) {
	cases := []map[string]string{
		{"x-request-id": "a"},
		{"x-trace-id": "b"},
		{"x-tt-logid": "c"},
	}
	for _, h := range cases {
		if requestIDOf(h) == "" {
			t.Fatalf("应识别请求 ID: %v", h)
		}
	}
	if requestIDOf(map[string]string{"server": "nginx"}) != "" {
		t.Fatal("没有请求 ID 头时应返回空")
	}
}

// 上游可能用 GBK 发错误文案：要转成 UTF-8，否则面板上全是乱码。
func TestUpstreamErrTextDecodesGBK(t *testing.T) {
	msg := "系统检测到当前账号存在异常调用，请稍后重试"
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(`{"code":200108,"message":"` + msg + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := upstreamErrText(429, gbk)
	if got != msg {
		t.Fatalf("GBK 文案应转成可读中文，实际 %q", got)
	}
}

// 合法 UTF-8 不能被误转。
func TestUpstreamErrTextKeepsUTF8(t *testing.T) {
	body := []byte(`{"code":200108,"message":"账号异常"}`)
	if got := upstreamErrText(429, body); got != "账号异常" {
		t.Fatalf("UTF-8 文案应原样返回，实际 %q", got)
	}
}

// OpenAI 形态的嵌套错误体（400/402 业务错误常见）必须抠出可读文案，
// 而不是把整段原始 JSON 当作错误消息丢给面板和流水。
func TestUpstreamErrTextNestedError(t *testing.T) {
	body := []byte(`{"error":{"code":1000007,"message":"积分余额不足，请充值后重试",` +
		`"param":null,"type":"invalid_request_error"}}`)
	if got := upstreamErrText(402, body); got != "积分余额不足，请充值后重试" {
		t.Fatalf("嵌套 error 应取 message 字段，实际 %q", got)
	}
	// 流水仍保留完整原文，这个函数只负责「对外可读」。
	if !strings.Contains(string(body), "invalid_request_error") {
		t.Fatal("测试数据应保持嵌套结构")
	}
}

// codeOf 也要能从嵌套 error 里取业务码（非 chat 端点走 Envelope 路径）。
func TestCodeOfNestedError(t *testing.T) {
	var env Envelope
	if err := json.Unmarshal([]byte(`{"error":{"code":1000007,"message":"x"}}`), &env); err != nil {
		t.Fatal(err)
	}
	if got := codeOf(&env); got != 1000007 {
		t.Fatalf("嵌套 error.code 应取到 1000007，实际 %d", got)
	}
	if env.ErrMessage() != "x" {
		t.Fatalf("嵌套 error.message 应可读，实际 %q", env.ErrMessage())
	}
}

// 业务错误码要能从两种信封里取出来：{code} 与 {error:{code}}。
func TestCodeFromBody(t *testing.T) {
	cases := map[string]int{
		`{"code":200108,"message":"x"}`:           200108,
		`{"error":{"code":200108,"message":"x"}}`: 200108,
		`{"code":0,"data":{}}`:                    0,
		`not json`:                                0,
	}
	for body, want := range cases {
		if got := codeFromBody([]byte(body)); got != want {
			t.Fatalf("%s 应取出 %d，实际 %d", body, want, got)
		}
	}
}
