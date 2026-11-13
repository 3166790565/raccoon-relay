package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client 是上游 HTTP 客户端。所有端点都是同一域名下的固定路径，基址可被 env 覆盖。
type Client struct {
	Base string
	HTTP *http.Client
}

func NewClient(base string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		Base: strings.TrimRight(base, "/"),
		HTTP: &http.Client{Timeout: timeout},
	}
}

// Envelope 是小浣熊统一的 {code, message, data} 信封。
type Envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
	// body 是原始响应体，供没有 data 字段的端点（模型目录历史版本）解析。
	body []byte
}

func (e *Envelope) ErrMessage() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Msg
}

func (e *Envelope) OK() bool { return e.Code == 0 || e.Code == 200 }

func (c *Client) url(path string) string { return c.Base + path }

func (c *Client) doJSON(ctx context.Context, method, path string, headers map[string]string, payload any) (int, *Envelope, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), body)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	var env Envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			return resp.StatusCode, nil, fmt.Errorf("上游返回非 JSON（HTTP %d）", resp.StatusCode)
		}
	}
	env.body = raw
	return resp.StatusCode, &env, nil
}

// ---- 端点 ----

const (
	PathChat        = "/api/web/llm/v2/chat/completions"
	PathCatalog     = "/api/web/llm/v2/model_catalog"
	PathBalance     = "/api/web/points/v1/balance"
	PathSetting     = "/api/web/office/v3/setting_info"
	PathSettingOrg  = "/api/web/org/user/office/v1/setting_info"
	PathGrant       = "/api/web/desktop/v1/login/points/grant"
	PathMobileGrant = "/api/web/mobile/v1/login/points/grant"
	PathRefresh     = "/api/web/auth/v1/refresh"
	PathUserInfo    = "/api/web/auth/v1/user_info"
	PathExchange    = "/api/web/auth/v1/login_with_authorization_code"
	PathBindDevice  = "/api/web/auth/v1/devices_current_bind"
)

// ---- 结果类型 ----

type Model struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ContextLength   int    `json:"contextLength"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
	SupportsImages  bool   `json:"supportsImages"`
}

type RefreshResult struct {
	Token        string
	RefreshToken string
	OfficeID     string
}

type TradeExchange struct {
	Token        string
	RefreshToken string
	OfficeID     string
}

// ---- 请求实现 ----

// Refresh 用 refresh_token 换新 access。响应不返回新 refresh 时保留旧值（协议支持旋转但不强制）。
func (c *Client) Refresh(ctx context.Context, idn Identity, refreshToken string) (*RefreshResult, error) {
	status, env, err := c.doJSON(ctx, http.MethodPost, PathRefresh, refreshHeaders(idn), map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, &UpstreamError{Status: status, Code: codeOf(env), Message: "刷新被拒（401）"}
	}
	if status == http.StatusBadRequest && codeOf(env) == 200822 {
		return nil, &UpstreamError{Status: status, Code: 200822, Message: "refresh_token 已被并发刷新替换"}
	}
	data := rawData(env)
	var d struct {
		AccessToken  string `json:"access_token"`
		AccessToken2 string `json:"accessToken"`
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		Refresh2     string `json:"refreshToken"`
		OfficeID     string `json:"office_identity"`
	}
	_ = json.Unmarshal(data, &d)
	tok := firstNonEmpty(d.AccessToken, d.AccessToken2, d.Token)
	if status >= 200 && status < 300 && tok != "" {
		next := firstNonEmpty(d.RefreshToken, d.Refresh2)
		if next == "" {
			next = refreshToken
		}
		return &RefreshResult{Token: tok, RefreshToken: next, OfficeID: d.OfficeID}, nil
	}
	return nil, &UpstreamError{Status: status, Code: codeOf(env), Message: errText(env, status)}
}

func (c *Client) Balance(ctx context.Context, token string, idn Identity) (int64, error) {
	status, env, err := c.doJSON(ctx, http.MethodGet, PathBalance, webHeaders(token, idn, ""), nil)
	if err != nil {
		return 0, err
	}
	if status == http.StatusUnauthorized {
		return 0, &UpstreamError{Status: status, Code: codeOf(env), Auth: true, Message: "凭证失效"}
	}
	code := codeOf(env)
	if code == 200003 || strings.Contains(strings.ToLower(errText(env, 0)), "authorization_verify_error") {
		return 0, &UpstreamError{Status: status, Code: code, Auth: true, Message: "凭证失效"}
	}
	if status < 200 || status >= 300 || env == nil || !env.OK() {
		return 0, &UpstreamError{Status: status, Code: code, Message: "额度查询失败：" + errText(env, status)}
	}
	var d struct {
		AvailablePoints  *int64 `json:"available_points"`
		AvailablePoints2 *int64 `json:"availablePoints"`
	}
	_ = json.Unmarshal(rawData(env), &d)
	if d.AvailablePoints != nil {
		return *d.AvailablePoints, nil
	}
	if d.AvailablePoints2 != nil {
		return *d.AvailablePoints2, nil
	}
	return 0, nil
}

// Catalog 拉官方模型目录。
func (c *Client) Catalog(ctx context.Context, token string, idn Identity) ([]Model, error) {
	status, env, err := c.doJSON(ctx, http.MethodGet, PathCatalog, webHeaders(token, idn, ""), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, &UpstreamError{Status: status, Auth: true, Message: "登录态已过期（HTTP 401）"}
	}
	if status < 200 || status >= 300 {
		return nil, &UpstreamError{Status: status, Message: fmt.Sprintf("目录拉取失败（HTTP %d）", status)}
	}
	return parseCatalog(payloadOf(env, env.body)), nil
}

// parseCatalog 兼容 {models:[...]} 与 {categories:[{type:"chat",models:[...]}]} 两种形状。
func parseCatalog(raw json.RawMessage) []Model {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	type item struct {
		Name           string `json:"name"`
		ID             string `json:"id"`
		Model          string `json:"model"`
		ModelName      string `json:"model_name"`
		DisplayName    string `json:"display_name"`
		Description    string `json:"description"`
		Visible        *bool  `json:"visible"`
		ContextWindow  int    `json:"context_window"`
		MaxTokens      int    `json:"max_tokens"`
		SupportsImages *bool  `json:"supports_images"`
		SupportImage   *bool  `json:"supportImage"`
		SupportsReason *bool  `json:"supports_reasoning"`
		Params         struct {
			ContextWindow int `json:"context_window"`
			MaxTokens     int `json:"max_tokens"`
		} `json:"params"`
		Tags []string `json:"tags"`
	}
	var out []Model
	seen := map[string]bool{}

	collect := func(rawItems []byte) {
		var items []item
		if err := json.Unmarshal(rawItems, &items); err != nil {
			return
		}
		for _, it := range items {
			id := firstNonEmpty(it.Name, it.ID, it.Model, it.ModelName)
			if id == "" || seen[id] {
				continue
			}
			if it.Visible != nil && !*it.Visible {
				continue
			}
			seen[id] = true
			ctxLen := firstPositive(it.ContextWindow, it.Params.ContextWindow)
			maxOut := firstPositive(it.MaxTokens, it.Params.MaxTokens)
			imgs := false
			if it.SupportsImages != nil {
				imgs = *it.SupportsImages
			} else if it.SupportImage != nil {
				imgs = *it.SupportImage
			} else {
				for _, t := range it.Tags {
					lt := strings.ToLower(t)
					if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
						imgs = true
						break
					}
				}
			}
			display := firstNonEmpty(it.DisplayName, shortDesc(it.Description), id)
			out = append(out, Model{ID: id, Name: display, ContextLength: ctxLen, MaxOutputTokens: maxOut, SupportsImages: imgs})
		}
	}

	if rawModels, ok := root["models"]; ok {
		collect(rawModels)
	}
	if out == nil {
		var cats []struct {
			Type   string          `json:"type"`
			Models json.RawMessage `json:"models"`
		}
		if rawCats, ok := root["categories"]; ok {
			if err := json.Unmarshal(rawCats, &cats); err == nil {
				for _, cat := range cats {
					if cat.Type != "" && cat.Type != "chat" {
						continue
					}
					collect(cat.Models)
				}
			}
		}
	}
	return out
}

// CheckinResult 是签到两步的结论。Already=true 表示今日已领（幂等，不是失败）。
type CheckinResult struct {
	OK      bool
	Claimed bool
	Already bool
	Message string
}

// Checkin 严格两步：先 setting_info 激活当日 Web 会话（失败不阻断），再 grant 领取。
func (c *Client) Checkin(ctx context.Context, token string, idn Identity) (*CheckinResult, error) {
	settingPath := PathSetting
	if isTeam(idn.OfficeIdentity) {
		settingPath = PathSettingOrg
	}
	// 第 1 步失败不阻断，由 grant 自行暴露
	_, _, _ = c.doJSON(ctx, http.MethodGet, settingPath, webHeaders(token, idn, "web"), nil)

	status, env, err := c.doJSON(ctx, http.MethodPost, PathGrant, webHeaders(token, idn, ""), map[string]any{})
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, &UpstreamError{Status: status, Code: codeOf(env), Auth: true, Message: "凭证失效，请重新登录"}
	}
	if status < 200 || status >= 300 || env == nil || !env.OK() {
		return &CheckinResult{OK: false, Message: errText(env, status)}, nil
	}
	var d struct {
		Granted bool `json:"granted"`
	}
	_ = json.Unmarshal(rawData(env), &d)
	if d.Granted {
		return &CheckinResult{OK: true, Claimed: true, Message: "已领取每日积分"}, nil
	}
	return &CheckinResult{OK: true, Already: true, Message: "今日已领取过"}, nil
}

// ClaimOnboarding 领一次性首登奖。ledger 是账号 meta 里的已结算台账，键为任务名。
// 返回新增结算项，调用方写回 meta。桌面端与手机端两条端点各领一次、互不相通。
func (c *Client) ClaimOnboarding(ctx context.Context, token string, idn Identity, ledger map[string]int64) (map[string]int64, []string, error) {
	now := time.Now().UnixMilli()
	settled := map[string]int64{}
	var results []string
	tasks := []struct {
		key     string
		path    string
		headers map[string]string
	}{
		{"desktop_login_grant", PathGrant, webHeaders(token, idn, "")},
		{"mobile_login_grant", PathMobileGrant, mobileHeaders(token, idn)},
	}
	for _, t := range tasks {
		if v, ok := ledger[t.key]; ok && (v > 0 || v == -1) {
			continue
		}
		status, env, err := c.doJSON(ctx, http.MethodPost, t.path, t.headers, map[string]any{})
		if err != nil {
			continue // 网络失败不落台账，下次重试
		}
		if status == http.StatusUnauthorized {
			return settled, results, &UpstreamError{Status: status, Auth: true, Message: "凭证失效，请重新登录"}
		}
		if status == http.StatusNotFound || status == http.StatusGone {
			settled[t.key] = -1 // 端点已下线，永久停试
			results = append(results, t.key+": 端点已不存在")
			continue
		}
		if status < 200 || status >= 300 || env == nil || !env.OK() {
			continue // 失败不落台账
		}
		var d struct {
			Granted bool `json:"granted"`
			Popup   struct {
				Points int64 `json:"points"`
			} `json:"popup"`
		}
		_ = json.Unmarshal(rawData(env), &d)
		settled[t.key] = now
		if d.Granted {
			results = append(results, fmt.Sprintf("%s: 已领取 %d 积分", t.key, d.Popup.Points))
		} else {
			results = append(results, t.key+": 此前已领取")
		}
	}
	return settled, results, nil
}

// UserInfo 查账号信息，接口不可用时由调用方退回本地解 JWT。
func (c *Client) UserInfo(ctx context.Context, token string, idn Identity) (uid, name string) {
	status, env, err := c.doJSON(ctx, http.MethodGet, PathUserInfo, webHeaders(token, idn, ""), nil)
	if err != nil || status < 200 || status >= 300 || env == nil {
		return TokenUID(token), ""
	}
	var d struct {
		ID       any    `json:"id"`
		UserID   any    `json:"user_id"`
		UID      any    `json:"uid"`
		Name     string `json:"name"`
		Nickname string `json:"nickname"`
		UserName string `json:"user_name"`
	}
	_ = json.Unmarshal(rawData(env), &d)
	return firstNonEmpty(anyStr(d.ID), anyStr(d.UserID), anyStr(d.UID)), firstNonEmpty(d.Name, d.Nickname, d.UserName)
}

// ExchangeAuthCode 用一次性授权码换 token。
func (c *Client) ExchangeAuthCode(ctx context.Context, code string) (*TradeExchange, error) {
	status, env, err := c.doJSON(ctx, http.MethodPost, PathExchange,
		map[string]string{"content-type": "application/json"}, map[string]string{"authorization_code": code})
	if err != nil {
		return nil, err
	}
	code2 := codeOf(env)
	if status == http.StatusOK && code2 == 200035 {
		return nil, &UpstreamError{Status: status, Code: 200035, Message: "授权码已失效或已被消费（一次性），请回到官方授权页重新登录获取新码"}
	}
	data := rawData(env)
	var d struct {
		AccessToken  string `json:"access_token"`
		AccessToken2 string `json:"accessToken"`
		RefreshToken string `json:"refresh_token"`
		Refresh2     string `json:"refreshToken"`
		OfficeID     string `json:"office_identity"`
	}
	_ = json.Unmarshal(data, &d)
	tok := firstNonEmpty(d.AccessToken, d.AccessToken2)
	if status < 200 || status >= 300 || tok == "" {
		return nil, &UpstreamError{Status: status, Code: code2, Message: errText(env, status)}
	}
	return &TradeExchange{
		Token:        tok,
		RefreshToken: firstNonEmpty(d.RefreshToken, d.Refresh2),
		OfficeID:     d.OfficeID,
	}, nil
}

// BindDevice 注册可信设备。失败不阻断入池（只是少一层设备信任）。
func (c *Client) BindDevice(ctx context.Context, token, deviceID string) error {
	if token == "" || deviceID == "" {
		return nil
	}
	seed := hashHex(deviceID)
	name := "DESKTOP-" + strings.ToUpper(sanitizeAlnum(seed[:7]))
	payload := map[string]any{
		"client_device_id_source": "unknown",
		"client_device_id":        deviceID,
		"client_platform":         "desktop-windows",
		"client_version":          ClientVersion,
		"device_name":             name,
		"os":                      "windows",
		"os_version":              DefaultOSVersion,
		"application":             "desktop",
		"push_permission":         "unknown",
	}
	status, env, err := c.doJSON(ctx, http.MethodPost, PathBindDevice, bindHeaders(token, deviceID), payload)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &UpstreamError{Status: status, Code: codeOf(env), Message: errText(env, status)}
	}
	return nil
}

// ---- 错误与工具 ----

type UpstreamError struct {
	Status  int
	Code    int
	Auth    bool
	Message string
}

func (e *UpstreamError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("上游错误 HTTP %d", e.Status)
}

func codeOf(env *Envelope) int {
	if env == nil {
		return 0
	}
	return env.Code
}

func errText(env *Envelope, status int) string {
	if env != nil {
		if m := env.ErrMessage(); m != "" {
			return m
		}
	}
	if status != 0 {
		return fmt.Sprintf("HTTP %d", status)
	}
	return ""
}

func rawData(env *Envelope) json.RawMessage {
	if env == nil {
		return nil
	}
	if len(env.Data) > 0 && string(env.Data) != "null" && string(env.Data) != "[]" {
		return env.Data
	}
	return nil
}

// payloadOf 返回解析用的数据：优先 {code,data} 信封的 data；data 缺失时退回归一化的整个响应体。
// 有些端点（如模型目录的历史版本）不带信封直接返回数据本体。
func payloadOf(env *Envelope, body []byte) json.RawMessage {
	if d := rawData(env); d != nil {
		return d
	}
	if len(body) > 0 {
		return body
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func shortDesc(s string) string {
	if len([]rune(s)) <= 64 {
		return s
	}
	return ""
}

func anyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	case json.Number:
		return t.String()
	}
	return ""
}
