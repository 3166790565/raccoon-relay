package raccoon

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodeFromBodyExNamedCode(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantName string
	}{
		{"数字码", `{"code":1000007,"message":"x"}`, 1000007, ""},
		{"数字字符串码", `{"code":"1000007"}`, 1000007, ""},
		{"名字码", `{"code":"insufficient_points","message":"x"}`, 0, "insufficient_points"},
		{"嵌套 error.code", `{"error":{"code":"arrearage"}}`, 0, "arrearage"},
		{"嵌套 type", `{"error":{"type":"rate_limit_exceeded","message":"m"}}`, 0, "rate_limit_exceeded"},
		{"无码", `{"message":"x"}`, 0, ""},
		{"空体", ``, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, name := codeFromBodyEx([]byte(c.body))
			if code != c.wantCode || name != c.wantName {
				t.Fatalf("期望 (%d,%q)，实际 (%d,%q)", c.wantCode, c.wantName, code, name)
			}
		})
	}
}

func TestIsQuotaCode(t *testing.T) {
	if !IsQuotaCode(1000007, "") {
		t.Fatal("1000007 是积分不足")
	}
	if !IsQuotaCode(0, "insufficient_points") || !IsQuotaCode(0, "ARREARAGE") {
		t.Fatal("名字码应大小写不敏感地命中")
	}
	if IsQuotaCode(0, "rate_limit") {
		t.Fatal("限流不是积分不足")
	}
}

func TestIsAuthCode(t *testing.T) {
	if !IsAuthCode(200003, "") || !IsAuthCode(200009, "") {
		t.Fatal("200003 / 200009 都是凭证失效")
	}
	if !IsAuthCode(0, "authorization_verify_error") {
		t.Fatal("文本码应命中")
	}
	if IsAuthCode(200108, "") {
		t.Fatal("200108 是账号异常，不是凭证失效")
	}
}

func TestIsContentPolicy(t *testing.T) {
	for _, msg := range []string{
		"Blocked by security policy",
		"request hit unapproved channel",
		"ILLEGAL API INVOCATION",
	} {
		if !IsContentPolicy(msg) {
			t.Fatalf("%q 应命中内容策略", msg)
		}
	}
	if IsContentPolicy("rate limit exceeded") {
		t.Fatal("限流不该命中内容策略")
	}
}

// 流里出现字符串码时不能再被当成坏帧丢弃。
func TestStreamFrameNamedCode(t *testing.T) {
	var f streamFrame
	raw := `{"error":{"code":"insufficient_points","message":"积分不足"}}`
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("带字符串 code 的帧必须能解析：%v", err)
	}
	ue := f.errorOf()
	if ue == nil {
		t.Fatal("应识别为错误帧")
	}
	if ue.CodeName != "insufficient_points" {
		t.Fatalf("应保留文本码，实际 %q", ue.CodeName)
	}
	if ue.Code != 0 || ue.Status != 0 {
		t.Fatalf("文本码不应伪造数字码或状态码，实际 code=%d status=%d", ue.Code, ue.Status)
	}
}

func TestStreamFrameNumericCode(t *testing.T) {
	var f streamFrame
	if err := json.Unmarshal([]byte(`{"code":1000007,"message":"积分余额不足"}`), &f); err != nil {
		t.Fatal(err)
	}
	ue := f.errorOf()
	if ue == nil || ue.Code != 1000007 {
		t.Fatalf("数字码应正常解析，实际 %+v", ue)
	}
}

func TestStreamFrameDataFrameIsNotError(t *testing.T) {
	var f streamFrame
	if err := json.Unmarshal([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`), &f); err != nil {
		t.Fatal(err)
	}
	if ue := f.errorOf(); ue != nil {
		t.Fatalf("正常数据帧不该判成错误：%+v", ue)
	}
}

// 嵌套错误体要抠出文案，而不是把整段 JSON 塞进面板。
func TestUpstreamErrTextPrefersNestedMessage(t *testing.T) {
	body := []byte(`{"error":{"message":"内容包含敏感内容"}}`)
	if got := upstreamErrText(400, body); got != "内容包含敏感内容" {
		t.Fatalf("应取嵌套 message，实际 %q", got)
	}
	if got := upstreamErrText(400, []byte(`{"error":"plain text error"}`)); got != "plain text error" {
		t.Fatalf("error 为字符串时应原样取，实际 %q", got)
	}
	if got := upstreamErrText(403, []byte("<html>blocked</html>")); !strings.Contains(got, "blocked") {
		t.Fatalf("非 JSON 体应原样截断展示，实际 %q", got)
	}
}
