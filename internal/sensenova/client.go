// Package sensenova 实现商汤日日新（SenseNova / 日日新 Token Plan）的账号接入。
//
// 与「小浣熊」是两套完全不同的上游：
//
//   - 推理：https://token.sensenova.cn/v1（OpenAI 兼容），鉴权是 API Key；
//   - 额度：platform.sensenova.cn 控制台的 OAuth 登录票 + 积分池接口；
//   - API Key 本身也只能通过控制台登录票在 IAM 上创建（控制台「API-Key 管理」）。
//
// 所以一个日日新账号 = 控制台账号密码（用于换登录票、建 Key、读额度）
// + 一枚 API Key（用于推理）。这三点与 magpie-sensenova 插件、
// Agent2API 的 sensenova_auth.rs 逐条对齐。
package sensenova

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// 上游固定地址（照抄控制台前端配置与 Agent2API 的 sensenova_auth.rs）。
const (
	PlatformBase = "https://platform.sensenova.cn"
	PlatformLite = "https://platform.sensenova.cn/lite"
	IAMBase      = "https://iam.sensecoreapi.cn"
	IAMAuthN     = "https://iam.sensecoreapi.cn/iam/authn"
	ClientID     = "nova"
	// TokenPlanBase 是 OpenAI 兼容推理地址，模型请求与 /models 都打这里。
	TokenPlanBase = "https://token.sensenova.cn/v1"
	// OAuthScope 是网页端授权时的原值。
	OAuthScope = "openid offline offline_access"
	// UserAgent：登录页面对非常规 UA 有风控，固定一个普通浏览器 UA。
	UserAgent = "Mozilla/5.0"
	// maxRedirects 与源实现一致。
	maxRedirects = 12
)

// Client 是日日新上游 HTTP 客户端。
type Client struct {
	// HTTP 非流式请求用；对话流另走 StreamHTTP 以允许长连接。
	HTTP *http.Client
	// RequestTimeout 管登录/额度/建 Key 这类控制面请求。
	RequestTimeout time.Duration
	// IdleTimeout 对对话流生效：多久没收到上游数据算超时。
	IdleTimeout time.Duration

	// 可覆盖的基址，测试时指向 httptest。
	Platform string
	Lite     string
	IAM      string
	Token    string

	// modelsMu 缓存实时模型清单，避免每次 /v1/models 都打上游。
	modelsMu  sync.Mutex
	models    []string
	modelsAt  time.Time
	modelsTTL time.Duration
}

// NewClient 构造默认客户端。
func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		HTTP:           &http.Client{Timeout: timeout},
		RequestTimeout: timeout,
		IdleTimeout:    timeout,
		Platform:       PlatformBase,
		Lite:           PlatformLite,
		IAM:            IAMBase,
		Token:          TokenPlanBase,
		modelsTTL:      10 * time.Minute,
	}
}

/* ------------------------------------------------------------------ 错误 */

// Error 是控制面（登录/额度/建 Key）的人可读错误。网络与状态码细节放 Detail。
type Error struct {
	Op     string
	Msg    string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Msg + "：" + e.Detail
	}
	return e.Msg
}

func newError(op, msg, detail string) *Error { return &Error{Op: op, Msg: msg, Detail: detail} }

// UpstreamError 是推理面的错误，复用 raccoon.UpstreamError 让转发层的
// classify / 冷却 / 对外文案一套逻辑通吃。日日新的业务码可能是字符串
// （insufficient_quota 等），在解析时折成与小浣熊一致的数值直觉：
//   - insufficient_quota → 1000007（与小浣熊「积分不足」同义）
//   - invalid_api_key / authentication_error → Auth=true
//   - rate_limit_exceeded → HTTP 429

/* ------------------------------------------------------------ 工具函数 */

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b64url(b)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pkce 生成 code_verifier 与 S256 challenge。
func pkce() (verifier, challenge string) {
	verifier = randB64(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, b64url(sum[:])
}

func limitReader(body io.Reader) []byte {
	raw, _ := io.ReadAll(io.LimitReader(body, 1<<20))
	return raw
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// jwtPayload 解析 JWT 的载荷段；不透明 token 返回 nil。
func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func jwtString(payload map[string]any, keys ...string) string {
	cur := any(payload)
	for _, key := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	switch v := cur.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return fmt.Sprintf("%.0f", v)
	case json.Number:
		return v.String()
	}
	return ""
}

// TokenInfo 是从登录票里读出来的账号归属信息。
type TokenInfo struct {
	TenantID string
	Username string
	Exp      int64 // 秒级；0 表示不可知
}

// ParseToken 解出 tenant_id / username / exp。
func ParseToken(token string) TokenInfo {
	p := jwtPayload(token)
	if p == nil {
		return TokenInfo{}
	}
	info := TokenInfo{
		TenantID: jwtString(p, "ext", "tenant_id"),
		Username: jwtString(p, "ext", "username"),
	}
	if info.TenantID == "" {
		info.TenantID = jwtString(p, "tenant_id")
	}
	if exp, ok := p["exp"].(float64); ok && exp > 0 {
		info.Exp = int64(exp)
	}
	return info
}

// TokenReusable 判断登录票是否还能用（提前 60 秒算过期）。
func TokenReusable(token string, now time.Time) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	info := ParseToken(token)
	if info.Exp <= 0 {
		return false // 解不出 exp 就当作不能用，宁多重登一次
	}
	return now.Unix() < info.Exp-60
}

/* ------------------------------------------------------------ cookie jar */

// cookieJar 只认 name=value、同名覆盖。这条登录链路里的 Cookie 都是同一
// 站点的会话票，不需要处理 Domain/Path 与过期（与源实现一致的取舍）。
type cookieJar struct {
	mu    sync.Mutex
	pairs map[string]string
	order []string
}

func newCookieJar() *cookieJar { return &cookieJar{pairs: map[string]string{}} }

func (j *cookieJar) absorb(res *http.Response) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, raw := range res.Header.Values("Set-Cookie") {
		pair := strings.SplitN(raw, ";", 2)[0]
		i := strings.Index(pair, "=")
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(pair[:i])
		value := strings.TrimSpace(pair[i+1:])
		if name == "" {
			continue
		}
		if _, ok := j.pairs[name]; !ok {
			j.order = append(j.order, name)
		}
		j.pairs[name] = value
	}
}

func (j *cookieJar) header() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	parts := make([]string, 0, len(j.order))
	for _, name := range j.order {
		parts = append(parts, name+"="+j.pairs[name])
	}
	return strings.Join(parts, "; ")
}

/* ------------------------------------------------------------------ 登录 */

// LoginResult 是一次控制台登录的结果。
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    int64 // 毫秒；0 表示不可知
	TenantID     string
	Username     string
}

// Login 用控制台账号密码换登录票（OAuth2 授权码 + PKCE）。
//
// 四步与 Agent2API / magpie 的实现逐条一致：
//  1. GET 授权页并跟随跳转，停在登录页时从地址里取 login_challenge；
//  2. POST IAM nova/login 提交账号密码；
//  3. 跟随 redirect 链直到出现 code=；
//  4. POST oauth2/token 换 access_token。
func (c *Client) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, newError("login", "账号或密码为空", "")
	}
	verifier, challenge := pkce()
	state := randHex(16)
	jar := newCookieJar()

	authURL := c.Platform + "/oauth2/auth?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {ClientID},
		"code_challenge_method": {"S256"},
		"code_challenge":        {challenge},
		"redirect_uri":          {c.Platform},
		"scope":                 {OAuthScope},
		"state":                 {state},
		"lang":                  {"zh-CN"},
	}.Encode()

	final, status, err := c.follow(ctx, jar, authURL)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, newError("login", "授权页返回异常", fmt.Sprintf("HTTP %d", status))
	}
	if detail := queryParam(final, "error_description"); detail != "" {
		return nil, newError("login", "授权页拒绝了登录请求", shorten(detail, 160))
	}
	challengeValue := queryParam(final, "login_challenge")
	if challengeValue == "" {
		if code := queryParam(final, "error"); code != "" {
			return nil, newError("login", "授权页拒绝了登录请求", shorten(code, 160))
		}
		return nil, newError("login", "未能从授权页取到登录 challenge", shorten(final, 120))
	}

	// 登录前先确认 challenge 还有效；过期了再提交账号密码只会白耗一次失败次数。
	checkURL := c.IAM + "/iam/authn/v1/auth/checkChallenge?challenge=" + url.QueryEscape(challengeValue)
	checkReq, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return nil, err
	}
	checkReq.Header.Set("Accept", "application/json")
	checkReq.Header.Set("User-Agent", UserAgent)
	checkRes, err := c.httpClient().Do(checkReq)
	if err == nil {
		checkRaw := limitReader(checkRes.Body)
		checkRes.Body.Close()
		jar.absorb(checkRes)
		var checkData struct {
			IsValid *bool `json:"is_valid"`
		}
		_ = json.Unmarshal(checkRaw, &checkData)
		if checkData.IsValid != nil && !*checkData.IsValid {
			return nil, newError("login", "登录链接已过期，请重试", "")
		}
	}

	// 与 magpie-sensenova / Agent2API 一致：明文密码 + is_encrypt:false。
	loginBody, _ := json.Marshal(map[string]any{
		"username":   username,
		"password":   password,
		"challenge":  challengeValue,
		"is_encrypt": false,
	})
	loginReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.IAM+"/iam/authn/v1/auth/nova/login", strings.NewReader(string(loginBody)))
	if err != nil {
		return nil, err
	}
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("Accept", "application/json")
	loginReq.Header.Set("User-Agent", UserAgent)
	loginReq.Header.Set("Origin", c.Platform)
	loginReq.Header.Set("Referer", c.Platform+"/login")
	if ck := jar.header(); ck != "" {
		loginReq.Header.Set("Cookie", ck)
	}
	loginRes, err := c.httpClient().Do(loginReq)
	if err != nil {
		return nil, newError("login", "登录接口请求失败", err.Error())
	}
	jar.absorb(loginRes)
	raw := limitReader(loginRes.Body)
	loginRes.Body.Close()
	var loginData struct {
		Redirect string `json:"redirect"`
		Message  string `json:"message"`
		Details  []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"details"`
	}
	_ = json.Unmarshal(raw, &loginData)
	if loginRes.StatusCode >= 400 {
		return nil, newError("login", "账号登录失败", loginReason(loginData.Details, loginData.Message, loginRes.StatusCode))
	}
	redirect := strings.TrimSpace(loginData.Redirect)
	if redirect == "" {
		return nil, newError("login", "登录接口没有返回跳转地址", "")
	}

	start := redirect
	if u, err := url.Parse(redirect); err == nil && !u.IsAbs() {
		start = c.Platform + redirect
	}
	if code := queryParam(start, "code"); code != "" {
		return c.exchangeCode(ctx, jar, code, verifier, state)
	}
	final, status, err = c.follow(ctx, jar, start)
	if err != nil {
		return nil, err
	}
	code := queryParam(final, "code")
	if code == "" {
		return nil, newError("login", "登录后没有拿到一次性授权码", fmt.Sprintf("HTTP %d", status))
	}
	return c.exchangeCode(ctx, jar, code, verifier, state)
}

func loginReason(details []struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}, message string, status int) string {
	var reason, detail string
	for _, d := range details {
		if reason == "" && d.Reason != "" {
			reason = d.Reason
		}
		if detail == "" && d.Message != "" {
			detail = d.Message
		}
	}
	known := map[string]string{
		"incorrectUsernameOrPassword": "账号或密码不正确",
		"invalidAccountOrPassword":    "账号或密码不正确",
		"IncorrectPassword":           "密码不正确",
		"NotFound":                    "账号不存在",
		"LoginLocked":                 "账号被锁定，请 15 分钟后再试",
		"locked":                      "账号被锁定，请 15 分钟后再试",
		"forbidLoginForMoment":        "账号被锁定，请 15 分钟后再试",
		"PermissionDenied":            "该账号没有权限",
		"OperationTooFrequently":      "操作过于频繁，请稍后再试",
	}
	// forbidLoginForMoment 与 PermissionDenied 共用 reason=PermissionDenied，
	// 只能靠 message 区分：锁定期间账号密码根本没被校验。
	if strings.Contains(strings.ToLower(detail+" "+message), "has been locked") {
		return "账号被锁定，请 15 分钟后再试"
	}
	if text, ok := known[reason]; ok {
		return text
	}
	if detail != "" {
		return detail
	}
	if message != "" {
		return message
	}
	return fmt.Sprintf("HTTP %d", status)
}

// exchangeCode 用授权码换登录票。
func (c *Client) exchangeCode(ctx context.Context, jar *cookieJar, code, verifier, state string) (*LoginResult, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {c.Platform},
		"code_verifier": {verifier},
		"state":         {state},
	}
	res, err := c.postForm(ctx, jar, c.Platform+"/oauth2/token", form)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 || res.AccessToken == "" {
		detail := res.ErrorDescription
		if detail == "" {
			detail = res.Error
		}
		if detail == "" {
			detail = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return nil, newError("login", "换取登录票失败", shorten(detail, 160))
	}
	info := ParseToken(res.AccessToken)
	exp := res.ExpiresAt
	if exp == 0 && info.Exp > 0 {
		exp = info.Exp * 1000
	}
	return &LoginResult{
		AccessToken: res.AccessToken, RefreshToken: res.RefreshToken, IDToken: res.IDToken,
		ExpiresAt: exp, TenantID: info.TenantID, Username: info.Username,
	}, nil
}

// Refresh 用 refresh_token 换新登录票。
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*LoginResult, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, newError("refresh", "没有 refresh_token", "")
	}
	jar := newCookieJar()
	res, err := c.postForm(ctx, jar, c.Platform+"/oauth2/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 || res.AccessToken == "" {
		detail := res.ErrorDescription
		if detail == "" {
			detail = res.Error
		}
		if detail == "" {
			detail = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return nil, newError("refresh", "登录票续期失败", shorten(detail, 160))
	}
	info := ParseToken(res.AccessToken)
	exp := res.ExpiresAt
	if exp == 0 && info.Exp > 0 {
		exp = info.Exp * 1000
	}
	next := res.RefreshToken
	if next == "" {
		next = refreshToken // 协议支持旋转但不强制
	}
	return &LoginResult{
		AccessToken: res.AccessToken, RefreshToken: next, IDToken: res.IDToken,
		ExpiresAt: exp, TenantID: info.TenantID, Username: info.Username,
	}, nil
}

type tokenResponse struct {
	StatusCode       int
	AccessToken      string
	RefreshToken     string
	IDToken          string
	ExpiresAt        int64
	Error            string
	ErrorDescription string
}

func (c *Client) postForm(ctx context.Context, jar *cookieJar, target string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if ck := jar.header(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	res, err := c.httpClient().Do(req)
	if err != nil {
		return nil, newError("token", "登录票请求失败", err.Error())
	}
	defer res.Body.Close()
	jar.absorb(res)
	raw := limitReader(res.Body)
	var data struct {
		AccessToken      string  `json:"access_token"`
		RefreshToken     string  `json:"refresh_token"`
		IDToken          string  `json:"id_token"`
		ExpiresIn        float64 `json:"expires_in"`
		Error            string  `json:"error"`
		ErrorDescription string  `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &data)
	out := &tokenResponse{
		StatusCode: res.StatusCode, AccessToken: strings.TrimSpace(data.AccessToken),
		RefreshToken: strings.TrimSpace(data.RefreshToken), IDToken: strings.TrimSpace(data.IDToken),
		Error: data.Error, ErrorDescription: data.ErrorDescription,
	}
	if data.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(data.ExpiresIn) * time.Second).UnixMilli()
	}
	return out, nil
}

// follow 手动跟跳转，每步带 Cookie、每步收 Set-Cookie。返回最终地址与状态码。
func (c *Client) follow(ctx context.Context, jar *cookieJar, start string) (string, int, error) {
	current := start
	for i := 0; i <= maxRedirects; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return current, 0, err
		}
		req.Header.Set("Accept", "text/html,application/json")
		req.Header.Set("User-Agent", UserAgent)
		if ck := jar.header(); ck != "" {
			req.Header.Set("Cookie", ck)
		}
		// 必须自己跟跳转：默认客户端会把 302 自动吃掉，
		// 我们就看不到中间的 Location（login_challenge 只在那一跳里）。
		res, err := c.redirectlessClient().Do(req)
		if err != nil {
			return current, 0, newError("login", "登录跳转请求失败", err.Error())
		}
		jar.absorb(res)
		loc := res.Header.Get("Location")
		status := res.StatusCode
		// 只读响应头，不读体；关掉 body 释放连接
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		res.Body.Close()
		if loc == "" || status < 300 || status >= 400 {
			return current, status, nil
		}
		next, err := resolveURL(current, loc)
		if err != nil {
			return current, status, err
		}
		current = next
	}
	return current, 0, newError("login", "登录跳转次数过多", "已超过上限")
}

func resolveURL(current, location string) (string, error) {
	base, err := url.Parse(current)
	if err != nil {
		return "", newError("login", "登录跳转地址无法解析", current)
	}
	if abs, err := url.Parse(location); err == nil && abs.IsAbs() {
		return abs.String(), nil
	}
	next, err := base.Parse(location)
	if err != nil {
		return "", newError("login", "登录跳转地址无法解析", location)
	}
	return next.String(), nil
}

func queryParam(rawURL, name string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get(name))
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	return c.HTTP
}

// redirectlessClient 复用同一个 Transport，但禁用自动跟跳转。
// follow 要逐步读 Location，交给默认 Transport 会只剩最终页。
func (c *Client) redirectlessClient() *http.Client {
	base := c.httpClient()
	return &http.Client{
		Transport: base.Transport,
		Timeout:   base.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

/* -------------------------------------------------------------- API Key */

// CreateAPIKey 用控制台登录票创建一枚 API Key，返回明文。
// 控制台的「API-Key 管理」走的也是这个接口，Key 的明文只在创建响应里出现一次。
func (c *Client) CreateAPIKey(ctx context.Context, consoleToken, name string) (string, error) {
	body, _ := json.Marshal(map[string]string{"displayname": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.IAM+"/iam/idp/v1/apiKeys", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+consoleToken)
	req.Header.Set("User-Agent", UserAgent)
	res, err := c.httpClient().Do(req)
	if err != nil {
		return "", newError("api-key", "创建 API Key 请求失败", err.Error())
	}
	defer res.Body.Close()
	raw := limitReader(res.Body)
	var data struct {
		APIKeyPlain string `json:"api_key_plain"`
		APIKey      string `json:"api_key"`
		Message     string `json:"message"`
	}
	_ = json.Unmarshal(raw, &data)
	key := firstNonEmpty(data.APIKeyPlain, data.APIKey)
	if res.StatusCode >= 400 || key == "" {
		detail := data.Message
		if detail == "" {
			detail = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return "", newError("api-key", "创建 API Key 失败", shorten(detail, 200))
	}
	return strings.TrimSpace(key), nil
}

// ListAPIKeys 返回控制台里已有 Key 的掩码清单，仅用于排障展示。
func (c *Client) ListAPIKeys(ctx context.Context, consoleToken string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.IAM+"/iam/idp/v1/apiKeys?page_size=50", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+consoleToken)
	req.Header.Set("Accept", "application/json")
	res, err := c.httpClient().Do(req)
	if err != nil {
		return 0, newError("api-key", "读取 API Key 列表失败", err.Error())
	}
	defer res.Body.Close()
	raw := limitReader(res.Body)
	if res.StatusCode >= 400 {
		return 0, newError("api-key", "读取 API Key 列表失败", fmt.Sprintf("HTTP %d", res.StatusCode))
	}
	var data struct {
		APIKeys []struct {
			APIKey string `json:"api_key"`
		} `json:"api_keys"`
		TotalSize int `json:"total_size"`
	}
	_ = json.Unmarshal(raw, &data)
	if data.TotalSize > 0 {
		return data.TotalSize, nil
	}
	return len(data.APIKeys), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// 让编译器确认 UpstreamError 仍然来自 raccoon（转发层按它分类）。
var _ = raccoon.UpstreamError{}
