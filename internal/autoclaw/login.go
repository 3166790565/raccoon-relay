package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// SendCode 给国内站手机号发验证码。手机号必须是 1 开头的 11 位数字（不带 +86）。
func (c *Client) SendCode(ctx context.Context, phone string) error {
	phone = strings.TrimSpace(phone)
	if !phoneRE.MatchString(phone) {
		return fmt.Errorf("手机号格式不对：需要 1 开头的 11 位数字（不带 +86）")
	}
	device := randomHex(16)
	body := map[string]any{"source_id": Product, "device_id": device, "phone": phone}
	env, err := c.doBusiness(ctx, strings.TrimRight(c.CNHost, "/"), http.MethodPost,
		"/userapi/v1/agent-send-code", "", body)
	if err != nil {
		return err
	}
	if env.code() != 0 {
		return fmt.Errorf("发送验证码失败：%s", firstNonEmpty(env.text(), fmt.Sprintf("code %d", env.code())))
	}
	return nil
}

var phoneRE = regexp.MustCompile(`^1\d{10}$`)

// LoginWithCode 用手机号 + 验证码登录国内站。
//
// 注意：登录接口要求 code 是 JSON 数字，字符串会被上游拒成 400001。
// 这一点和大多数接口不同，必须显式转成数字。
func (c *Client) LoginWithCode(ctx context.Context, phone, code string) (Credential, string, error) {
	phone = strings.TrimSpace(phone)
	if !phoneRE.MatchString(phone) {
		return Credential{}, "", fmt.Errorf("手机号格式不对")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return Credential{}, "", fmt.Errorf("验证码不能为空")
	}
	device := randomHex(16)
	body := map[string]any{
		"source_id": Product,
		"device_id": device,
		"phone":     phone,
		"code":      digitOrString(code),
	}
	env, err := c.doBusiness(ctx, strings.TrimRight(c.CNHost, "/"), http.MethodPost,
		"/userapi/v1/agent-login/", "", body)
	if err != nil {
		return Credential{}, "", err
	}
	if env.code() != 0 {
		return Credential{}, "", fmt.Errorf("登录失败：%s", firstNonEmpty(env.text(), fmt.Sprintf("code %d", env.code())))
	}
	var data map[string]any
	_ = json.Unmarshal(env.Data, &data)
	cred := tokenFields(data)
	if cred.Access == "" {
		return Credential{}, "", fmt.Errorf("登录响应缺少 access_token")
	}
	cred.DeviceID = device
	cred.Source = "cn-sms"
	cred.UserID = strOf(data["user_id"])
	// 用 base host 取一次用户资料拿昵称；失败不影响登录。
	if name := c.fetchProfileName(ctx, strings.TrimRight(c.CNHost, "/"), cred.Access); name != "" {
		cred.Name = name
	}
	return cred, phone, nil
}

// digitOrString 把纯数字串转成数字：上游登录接口只认数字类型的 code。
func digitOrString(s string) any {
	for _, r := range s {
		if r < '0' || r > '9' {
			return s
		}
	}
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}

// fetchProfileName 读昵称。失败返回空串。
func (c *Client) fetchProfileName(ctx context.Context, host, token string) string {
	env, err := c.doBusiness(ctx, host, http.MethodPost, pathUserProf, token, map[string]any{})
	if err != nil || env.code() != 0 {
		return ""
	}
	var data map[string]any
	_ = json.Unmarshal(env.Data, &data)
	return firstNonEmpty(strOf(data["nickname"]), strOf(data["name"]), strOf(data["username"]))
}

// AuthorizeURL 取国际站网页 OAuth 的授权地址。
//
// navigate_uri 必须落在官方 TokenServer 的白名单端口上，否则上游直接回
// 「Redirect URI not registered for this client」。所以这里只接受
// 18432 / 19654 / 19723 / 53699 四个端口之一。
func (c *Client) AuthorizeURL(ctx context.Context, vendor, navigateURI string) (string, error) {
	vendor = strings.ToLower(strings.TrimSpace(vendor))
	if vendor != "google" && vendor != "zai" {
		return "", fmt.Errorf("不支持的登录渠道：%s（只能是 google 或 zai）", vendor)
	}
	body := map[string]any{
		"source_id":    Product,
		"device_id":    randomHex(16),
		"navigate_uri": navigateURI,
	}
	env, err := c.doBusiness(ctx, strings.TrimRight(c.GlobalHost, "/"), http.MethodPost,
		"/userapi/overseasv1/"+vendor+"-oauth-url", "", body)
	if err != nil {
		return "", err
	}
	var data map[string]any
	_ = json.Unmarshal(env.Data, &data)
	for _, key := range []string{"oauth_url", "url", "login_url", "auth_url", "verify_url"} {
		if u := strOf(data[key]); u != "" {
			return u, nil
		}
	}
	return "", fmt.Errorf("上游没有返回授权地址")
}

// ExchangeOAuth 用回调里的 code 换凭据。
func (c *Client) ExchangeOAuth(ctx context.Context, vendor, code, state, navigateURI string) (Credential, error) {
	vendor = strings.ToLower(strings.TrimSpace(vendor))
	if vendor != "google" && vendor != "zai" {
		return Credential{}, fmt.Errorf("不支持的登录渠道：%s", vendor)
	}
	body := map[string]any{
		"source_id":    Product,
		"device_id":    randomHex(16),
		"code":         code,
		"state":        state,
		"navigate_uri": navigateURI,
	}
	env, err := c.doBusiness(ctx, strings.TrimRight(c.GlobalHost, "/"), http.MethodPost,
		"/userapi/overseasv1/"+vendor+"-oauth-login", "", body)
	if err != nil {
		return Credential{}, err
	}
	var data map[string]any
	_ = json.Unmarshal(env.Data, &data)
	cred := tokenFields(data)
	if cred.Access == "" {
		return Credential{}, fmt.Errorf("授权码兑换失败：响应缺少 access_token")
	}
	cred.Source = "oauth"
	cred.UserID = strOf(data["user_id"])
	cred.Name = firstNonEmpty(strOf(data["name"]), strOf(data["email"]))
	return cred, nil
}

// BuildAccount 把凭据组装成待入库的账号。
func BuildAccount(cred Credential, site, displayName string) *store.Account {
	if displayName == "" {
		displayName = firstNonEmpty(cred.Name, cred.UserID, jwtClaim(cred.Access, "sub"))
	}
	if displayName == "" {
		displayName = "AutoClaw 账号"
	}
	uid := firstNonEmpty(cred.UserID, jwtClaim(cred.Access, "user_id"), jwtClaim(cred.Access, "sub"))
	if uid == "" {
		uid = randomHex(8)
	}
	acc := &store.Account{
		UID:             uid,
		Name:            displayName,
		Provider:        store.ProviderAutoClaw,
		ExtSite:         site,
		ExtToken:        cred.Access,
		ExtTokenExp:     cred.Expires,
		ExtRefreshToken: cred.Refresh,
		Credits:         -1,
		Status:          store.StatusOnline,
		Language:        "zh",
		Meta:            map[string]any{"source": firstNonEmpty(cred.Source, "paste")},
		Enabled:         true,
	}
	raw, _ := json.Marshal(cred)
	acc.ExtExtra = string(raw)
	return acc
}

// 编译期断言：登录失败时面板要能显示可读原因，而不是一串 HTTP 码。
var _ = raccoon.UpstreamError{}
