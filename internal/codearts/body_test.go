package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 请求体整形：思考注入、输出预算封顶、stop 处理、强制流式、前缀剥离。
func TestPrepareChatBody(t *testing.T) {
	in := map[string]any{
		"model":      "codearts/GLM-5.2",
		"max_tokens": float64(999999),
		"stop":       "END",
		"stream":     false,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, opts := PrepareChatBody(in, store.StationCN)
	if out["model"] != "GLM-5.2" {
		t.Fatalf("前缀未剥离: %v", out["model"])
	}
	if n, _ := out["max_tokens"].(int); n != 65536 {
		t.Fatalf("输出预算未封顶: %v", out["max_tokens"])
	}
	if _, ok := out["stop"]; ok {
		t.Fatal("国内站必须删掉 stop")
	}
	if out["stream"] != true {
		t.Fatal("必须强制流式")
	}
	if !opts.Fold {
		t.Fatal("调用方要的是非流式，应标记 Fold")
	}
	th, _ := out["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Fatalf("应注入 thinking enabled，实际 %v", out["thinking"])
	}
}

// 客户端显式关闭思考时不能反被注入。
func TestPrepareChatBodyHonorsNoThinking(t *testing.T) {
	out, _ := PrepareChatBody(map[string]any{
		"model": "glm-5.3", "reasoning_effort": "off",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, store.StationGlobal)
	if _, ok := out["reasoning_effort"]; ok {
		t.Fatal("off 应被翻译成 thinking disabled 而不是原样下发")
	}
	th, _ := out["thinking"].(map[string]any)
	if th == nil || th["type"] != "disabled" {
		t.Fatalf("应注入 thinking disabled，实际 %v", out["thinking"])
	}
}

// 国际站接受字符串 stop 并归一成数组。
func TestPrepareChatBodyNormalizesStopOnInternational(t *testing.T) {
	out, _ := PrepareChatBody(map[string]any{
		"model": "glm-5.3", "stop": "END",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, store.StationGlobal)
	stops, ok := out["stop"].([]string)
	if !ok || len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop 应归一成字符串数组，实际 %v", out["stop"])
	}
}

// 超长工具名要截断，并能从响应里还原。
func TestShortenToolNames(t *testing.T) {
	long := strings.Repeat("a", 100)
	body := map[string]any{
		"model": "glm-5.3",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": long, "parameters": map[string]any{},
		}}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	_, opts := PrepareChatBody(body, store.StationGlobal)
	if len(opts.ToolNames) != 1 {
		t.Fatalf("应有 1 个截断映射，实际 %v", opts.ToolNames)
	}
	fn := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	short := fn["name"].(string)
	if len(short) != 64 {
		t.Fatalf("截断后长度应为 64，实际 %d", len(short))
	}
	if restoreName(short, opts.ToolNames) != long {
		t.Fatal("截断名无法还原")
	}
}

// DPoP proof 的签名能被内嵌公钥验证（raw r‖s，不是 ASN.1）。
func TestDPoPProofVerifies(t *testing.T) {
	jwk, err := GenerateDPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := signDPoP(jwk, "POST", "https://sts.example.com/v1/oauth2/tokens")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("JWS 应有 3 段，实际 %d", len(parts))
	}
	sig, err := b64urlDecode(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 64 {
		t.Fatalf("ES256 签名应为 64 字节 raw r||s，实际 %d", len(sig))
	}
	var j dpopJWK
	if err := json.Unmarshal([]byte(jwk), &j); err != nil {
		t.Fatal(err)
	}
	x, _ := b64urlDecode(j.X)
	y, _ := b64urlDecode(j.Y)
	pub := ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&pub, sum[:], r, s) {
		t.Fatal("DPoP 签名校验失败")
	}
}

// 错误码分类：限流 / 请求侧 / 凭据过期。
func TestEmbeddedErrorClassification(t *testing.T) {
	if got := embeddedErrorStatus("InferHub.ModelArts.81111.429"); got != 429 {
		t.Fatalf("限流码应映射 429，实际 %d", got)
	}
	if got := embeddedErrorStatus("InferHub.4004.200"); got != 400 {
		t.Fatalf("请求侧码应映射 400，实际 %d", got)
	}
	if got := embeddedErrorStatus("InferHub.9999.500"); got != 502 {
		t.Fatalf("其它码应映射 502，实际 %d", got)
	}
	if !IsStaleCredential(401, "") || !IsStaleCredential(200, `{"error_code":"APIG.0602"}`) {
		t.Fatal("401 / APIG.0602 应判为凭据过期")
	}
	if IsStaleCredential(400, "bad request") {
		t.Fatal("普通 400 不该判为凭据过期")
	}
}

// 会话号必须稳定且互相不同。
func TestAccountSessionIDsStable(t *testing.T) {
	c := Credential{SessionSeed: "seed-1"}
	s1, ch1 := AccountSessionIDs(c)
	s2, ch2 := AccountSessionIDs(c)
	if s1 != s2 || ch1 != ch2 {
		t.Fatal("同一账号的会话号必须稳定")
	}
	if s1 == ch1 || len(s1) != 32 {
		t.Fatalf("会话号与对话号应不同且为 32 位 hex，实际 %q / %q", s1, ch1)
	}
	other, _ := AccountSessionIDs(Credential{SessionSeed: "seed-2"})
	if other == s1 {
		t.Fatal("不同账号的会话号不该相同")
	}
}

// PKCE: challenge 是 verifier 的 SHA-256 base64url。
func TestGeneratePKCE(t *testing.T) {
	v, c := GeneratePKCE()
	sum := sha256.Sum256([]byte(v))
	if c != b64url(sum[:]) {
		t.Fatal("challenge 与 verifier 不匹配")
	}
	if v == "" || c == "" {
		t.Fatal("PKCE 不能为空")
	}
}
