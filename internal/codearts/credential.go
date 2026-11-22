package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"

	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// Credential 是 CodeArts 账号的完整凭据，整体存在账号的 ExtExtra 里。
//
// 它是「临时 AK/SK + 安全令牌 + 续期材料」的组合：
//   - 前三个字段签每一个业务请求；
//   - refresh_token / code_verifier / DPoP 私钥用于续期（三者缺一不可）；
//   - SessionSeed 决定账号在上游的稳定会话号（x-ot-session-id）。
type Credential struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	// ExpiresAt 是临时凭据的到期时刻（RFC3339 或毫秒时间戳字符串均可）。
	ExpiresAt    string `json:"expires_at"`
	RefreshToken string `json:"refresh_token,omitempty"`
	CodeVerifier string `json:"code_verifier,omitempty"`
	DPoPKey      string `json:"dpop_private_key_jwk,omitempty"`
	SessionSeed  string `json:"session_id,omitempty"`

	// 站点与身份（国际站才有 Station；身份用于 uid 与面板显示）。
	Station  string `json:"station,omitempty"`
	DomainID string `json:"domain_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	UserName string `json:"user_name,omitempty"`
	Phone    string `json:"user_phone,omitempty"`
	Source   string `json:"source,omitempty"`
}

// DecodeCred 从账号上还原凭据。
func DecodeCred(acc *store.Account) (Credential, error) {
	var c Credential
	if strings.TrimSpace(acc.ExtExtra) != "" {
		if err := json.Unmarshal([]byte(acc.ExtExtra), &c); err != nil {
			return c, fmt.Errorf("CodeArts 凭据解析失败: %w", err)
		}
	}
	if c.AccessKeyID == "" {
		// 兼容「只填了 AK/SK/ST 三元组」的最简形态。
		if strings.TrimSpace(acc.ExtToken) != "" {
			_ = json.Unmarshal([]byte(acc.ExtToken), &c)
		}
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return c, errors.New("CodeArts 账号没有可用的 AK/SK 凭据")
	}
	if c.RefreshToken == "" {
		c.RefreshToken = acc.ExtRefreshToken
	}
	if c.Station == "" {
		c.Station = acc.ExtSite
	}
	return c, nil
}

// Patch 把凭据写成账号补丁。
func (c Credential) Patch() store.AccountPatch {
	raw, _ := json.Marshal(c)
	exp := c.ExpiryMillis()
	site := c.Station
	if site == "" {
		site = store.StationGlobal
	}
	return store.AccountPatch{
		ExtToken:        strPtr(c.AccessKeyID + ":" + c.ExpiresAt),
		ExtTokenExp:     int64Ptr(exp),
		ExtRefreshToken: strPtr(c.RefreshToken),
		ExtExtra:        strPtr(string(raw)),
		ExtSite:         strPtr(site),
	}
}

// ExpiryMillis 解析临时凭据到期时刻，失败返回 0。
func (c Credential) ExpiryMillis() int64 {
	s := strings.TrimSpace(c.ExpiresAt)
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	// 上游有时给纯毫秒时间戳。
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err == nil {
		if v < 1e12 { // 秒级
			v *= 1000
		}
		return v
	}
	return 0
}

// NeedsRefresh 判断临时凭据是否临期。提前 REFRESH_LEAD（30 分钟）续期：
// 上游 STS 换来的三元组只有几小时，留足写回与失败重试的余量。
func (c Credential) NeedsRefresh(now time.Time) bool {
	exp := c.ExpiryMillis()
	if exp == 0 {
		return c.RefreshToken != "" // 不知道到期就先续一次
	}
	return now.Add(RefreshLead).UnixMilli() >= exp
}

// RefreshLead 是主动续期的提前量。
const RefreshLead = 30 * time.Minute

// ---------- DPoP ----------

// dpopJWK 是持久化的 DPoP 私钥（P-256）。
type dpopJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

// GenerateDPoPKey 生成一把 P-256 私钥并序列化成 JWK。
func GenerateDPoPKey() (string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	j := dpopJWK{
		Kty: "EC", Crv: "P-256",
		X: b64url(priv.PublicKey.X.Bytes()),
		Y: b64url(priv.PublicKey.Y.Bytes()),
		D: b64url(priv.D.Bytes()),
	}
	raw, _ := json.Marshal(j)
	return string(raw), nil
}

// signDPoP 生成一个 ES256 的 DPoP proof（JWS）。
//
// 头部内嵌公钥 JWK（不带 thumbprint 字段），载荷含 htm/htu/iat/jti。
// 签名是原始 r‖s 拼接，这正是 JWS 的 ES256 格式（Go 的 SignASN1 不是）。
func signDPoP(keyJWK, method, htu string) (string, error) {
	var j dpopJWK
	if err := json.Unmarshal([]byte(keyJWK), &j); err != nil {
		return "", fmt.Errorf("DPoP 私钥无法解析: %w", err)
	}
	x, err := b64urlDecode(j.X)
	if err != nil {
		return "", err
	}
	y, err := b64urlDecode(j.Y)
	if err != nil {
		return "", err
	}
	d, err := b64urlDecode(j.D)
	if err != nil {
		return "", err
	}
	priv := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)},
		D:         new(big.Int).SetBytes(d),
	}
	header := map[string]any{
		"alg": "ES256", "typ": "dpop+jwt",
		"jwk": map[string]string{"kty": "EC", "crv": "P-256", "x": j.X, "y": j.Y},
	}
	payload := map[string]any{
		"htm": strings.ToUpper(method),
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": randomHex(32),
	}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(payload)
	signingInput := b64url(hb) + "." + b64url(pb)
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	if err != nil {
		return "", err
	}
	// 固定 32 字节大端，短了要左侧补零，否则签名长度不对。
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + b64url(sig), nil
}
