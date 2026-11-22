package codearts

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// 国内站授权地址必须带齐插件认的字段，且 challenge 方法写 SHA-256。
func TestBuildAuthorizeURLMainland(t *testing.T) {
	u := BuildAuthorizeURL("cn", "challenge-1", "ticket-1", 18432)
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "codearts.huaweicloud.com" || parsed.Path != "/portal/authorize" {
		t.Fatalf("国内站授权地址不对: %s", u)
	}
	q := parsed.Query()
	for key, want := range map[string]string{
		"client_id":             "codearts-agent",
		"uri_scheme":            "codearts-agent",
		"code_challenge":        "challenge-1",
		"code_challenge_method": "SHA-256",
		"port":                  "18432",
		"plugin-name":           "snap_AIIDE",
		"ticket_id":             "ticket-1",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s=%q want %q", key, got, want)
		}
	}
}

// 国际站要先经过华为云国际登录页，授权地址被包在 service 参数里。
func TestBuildAuthorizeURLInternational(t *testing.T) {
	u := BuildAuthorizeURL("global", "challenge-1", "ticket-1", 18432)
	if !strings.HasPrefix(u, "https://auth.huaweicloud.com/authui/login?") {
		t.Fatalf("国际站应先走华为云国际登录页: %s", u)
	}
	parsed, _ := url.Parse(u)
	if parsed.Query().Get("site") != "intl" {
		t.Fatalf("缺少 site=intl: %s", u)
	}
	service := parsed.Query().Get("service")
	if !strings.Contains(service, "codearts.ap-southeast-1.huaweicloud.com/portal/authorize") {
		t.Fatalf("service 应指向国际站授权页: %s", service)
	}
}

// refresh token 的 user_profile 可能是对象，也可能是一段 base64 的 JSON。
func TestParseRefreshIdentity(t *testing.T) {
	profile := map[string]any{"principal_urn": "iam::dom:user:alice", "user_name": "alice"}
	payload := map[string]any{"user_profile": profile}
	jwt := fakeJWT(t, payload)
	urn, name := ParseRefreshIdentity(jwt)
	if urn != "iam::dom:user:alice" || name != "alice" {
		t.Fatalf("解析身份不对: urn=%q name=%q", urn, name)
	}
	if urn, _ := ParseRefreshIdentity("not-a-jwt"); urn != "" {
		t.Fatal("非 JWT 应返回空身份")
	}
}

func fakeJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(payload)
	return "x." + base64.RawURLEncoding.EncodeToString(raw) + ".y"
}

// 续期材料缺失时应明确报「需重新登录」，而不是发一个必然失败的请求。
func TestRefreshCredentialRequiresMaterials(t *testing.T) {
	c := NewClient(0)
	_, err := c.RefreshCredential(nil, Credential{RefreshToken: "only-refresh"})
	if err == nil {
		t.Fatal("缺少 code_verifier / DPoP 私钥时应报错")
	}
}
