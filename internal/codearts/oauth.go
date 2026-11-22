package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// tokenResponse 是 STS 换票的响应。凭据在 credentials 里，refresh_token 在外层。
type tokenResponse struct {
	Credentials *struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		Expiration      string `json:"expiration"`
	} `json:"credentials"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// requestToken 向 STS 换取临时凭据。
//
// 表单字段按 grant 区分：
//   - authorization_code：client_id / code / code_verifier / grant_type / redirect_uri
//   - refresh_token：client_id / code_verifier / grant_type / refresh_token
//
// 每次请求都要带一个 DPoP 头（ES256，内嵌公钥），htu 是 STS 的完整地址。
func (c *Client) requestToken(ctx context.Context, form url.Values, dpopKey string) (*tokenResponse, error) {
	proof, err := signDPoP(dpopKey, http.MethodPost, c.STS)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.STS, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("DPoP", proof)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{
		Transport: c.HTTP.Transport,
		Timeout:   c.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, &raccoon.UpstreamError{Status: 0, Message: "华为云 STS 连接失败: " + err.Error()}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out tokenResponse
	_ = json.Unmarshal(raw, &out)
	if res.StatusCode >= 400 || out.Credentials == nil {
		msg := firstNonEmpty(out.ErrorDesc, out.Error, strings.TrimSpace(string(raw)))
		ue := &raccoon.UpstreamError{Status: res.StatusCode, Message: "华为云 STS 换票失败：" + shorten(msg, 300)}
		// invalid_grant / 过期 / 已用 是终态，rail 层据此把号标成需重登。
		if out.Error == "invalid_grant" || DeadRefreshText(msg) {
			ue.Auth = true
		}
		if ue.Status == 0 {
			ue.Status = http.StatusBadGateway
		}
		return nil, ue
	}
	return &out, nil
}

// DeadRefreshText 判断续期失败是否终态（refresh_token 已作废）。
func DeadRefreshText(s string) bool {
	low := strings.ToLower(s)
	for _, w := range []string{"invalid refresh token", "refresh token has been used", "expiredrefreshtoken", "sts5.1806"} {
		if strings.Contains(low, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

// ExchangeCode 用授权码换第一条凭据。codeVerifier 与 dpopKey 必须与授权时一致。
func (c *Client) ExchangeCode(ctx context.Context, code, codeVerifier, dpopKey, redirectURI string) (Credential, error) {
	form := url.Values{
		"client_id":     {ClientID},
		"code":          {code},
		"code_verifier": {codeVerifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	res, err := c.requestToken(ctx, form, dpopKey)
	if err != nil {
		return Credential{}, err
	}
	return c.credentialFrom(res, codeVerifier, dpopKey), nil
}

// RefreshCredential 用 refresh_token 续期。保留身份与会话种子。
func (c *Client) RefreshCredential(ctx context.Context, cred Credential) (Credential, error) {
	if cred.RefreshToken == "" || cred.CodeVerifier == "" || cred.DPoPKey == "" {
		return cred, &raccoon.UpstreamError{Status: http.StatusUnauthorized, Auth: true,
			Message: "CodeArts 缺少续期材料（refresh_token / code_verifier / DPoP 私钥），请重新登录"}
	}
	form := url.Values{
		"client_id":     {ClientID},
		"code_verifier": {cred.CodeVerifier},
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.RefreshToken},
	}
	res, err := c.requestToken(ctx, form, cred.DPoPKey)
	if err != nil {
		return cred, err
	}
	next := c.credentialFrom(res, cred.CodeVerifier, cred.DPoPKey)
	// 续期响应偶尔不带新 refresh_token，沿用旧的。
	if next.RefreshToken == "" {
		next.RefreshToken = cred.RefreshToken
	}
	next.SessionSeed = cred.SessionSeed
	next.Station = cred.Station
	next.DomainID, next.UserID, next.UserName, next.Phone = cred.DomainID, cred.UserID, cred.UserName, cred.Phone
	next.Source = cred.Source
	return next, nil
}

func (c *Client) credentialFrom(res *tokenResponse, verifier, dpopKey string) Credential {
	cred := Credential{
		AccessKeyID:     res.Credentials.AccessKeyID,
		SecretAccessKey: res.Credentials.SecretAccessKey,
		SecurityToken:   res.Credentials.SecurityToken,
		ExpiresAt:       res.Credentials.Expiration,
		RefreshToken:    res.RefreshToken,
		CodeVerifier:    verifier,
		DPoPKey:         dpopKey,
		SessionSeed:     randomHex(16),
	}
	if cred.ExpiryMillis() == 0 {
		cred.ExpiresAt = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	}
	return cred
}

// BuildAuthorizeURL 拼登录授权地址（国内站直连；国际站走华为云国际登录页）。
func BuildAuthorizeURL(site, codeChallenge, ticketID string, port int) string {
	params := url.Values{
		"theme":                 {"2"},
		"locale":                {localeOf(site)},
		"uri_scheme":            {ClientID},
		"client_id":             {ClientID},
		"port":                  {fmt.Sprintf("%d", port)},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"SHA-256"},
		"ticket_id":             {ticketID},
		"plugin-name":           {LoginPlugin},
		"plugin-version":        {LoginPluginVer},
	}
	var portal string
	if site == "global" {
		portal = "https://codearts.ap-southeast-1.huaweicloud.com/portal/authorize"
		q := params.Encode()
		return "https://auth.huaweicloud.com/authui/login?site=intl&locale=en-us&service=" +
			url.QueryEscape(portal+"?"+q)
	}
	return "https://codearts.huaweicloud.com/portal/authorize?" + params.Encode()
}

func localeOf(site string) string {
	if site == "global" {
		return "en-us"
	}
	return "zh-cn"
}

// GeneratePKCE 生成 code_verifier 与 S256 challenge。
func GeneratePKCE() (verifier, challenge string) {
	verifier = b64url(randomBytes(48))
	sum := sha256Sum(verifier)
	return verifier, b64url(sum)
}

// ParseRefreshIdentity 从 refresh_token（JWT）里读账号身份。
func ParseRefreshIdentity(token string) (urn, name string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := b64urlDecode(parts[1])
	if err != nil {
		return "", ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", ""
	}
	profile, _ := m["user_profile"].(map[string]any)
	if profile == nil {
		return "", ""
	}
	urn = firstNonEmpty(strOf(profile["principal_urn"]), strOf(profile["account_name"]),
		strOf(profile["account_id"]), strOf(profile["user_id"]))
	name = firstNonEmpty(strOf(profile["user_name"]), strOf(profile["account_name"]))
	return urn, name
}
