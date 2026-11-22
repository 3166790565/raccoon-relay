package codearts

import (
	"testing"
	"time"
)

// 已知答案测试：向量直接取自插件的单元测试（index.test.mjs:165-178）。
// 它同时覆盖了三处最容易写错的细节：canonical URI 补斜杠、query 按值排序、
// 以及 extra 头（maas_type）参与签名。
func TestSignRequestKnownAnswer(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h, err := SignRequest(
		"AK", "SK", "ST",
		"POST",
		"https://example.com/api/v2/chat/completions?b=2&a=1",
		[]byte(`{"m":1}`),
		map[string]string{"maas_type": "benefit"},
		at,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "SDK-HMAC-SHA256 Access=AK," +
		"SignedHeaders=content-type;host;maas_type;x-sdk-content-sha256;x-sdk-date;x-security-token," +
		"Signature=f33e9f9e8fa23cb4dbac42f858e310657f72cc67179a098e0b3a2fa9a693e161"
	if h["Authorization"] != want {
		t.Fatalf("签名不匹配\n got: %s\nwant: %s", h["Authorization"], want)
	}
	if h["x-sdk-date"] != "20260101T000000Z" {
		t.Fatalf("x-sdk-date=%q", h["x-sdk-date"])
	}
	if _, ok := h["host"]; ok {
		t.Fatal("host 不应出现在返回头里（由传输层设置）")
	}
}

func TestCanonicalQuerySortsByKeyThenValue(t *testing.T) {
	got := canonicalQuery("b=2&a=2&a=1&c=%20x")
	want := "a=1&a=2&b=2&c=%20x"
	if got != want {
		t.Fatalf("canonical query=%q want %q", got, want)
	}
}

func TestRfc3986EscapesExtraChars(t *testing.T) {
	if got := rfc3986("a!'()*b~"); got != "a%21%27%28%29%2Ab~" {
		t.Fatalf("编码不对: %q", got)
	}
}
