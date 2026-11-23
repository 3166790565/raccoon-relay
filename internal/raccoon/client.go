package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client 是上游 HTTP 客户端。所有端点都是同一域名下的固定路径，基址可被 env 覆盖。
type Client struct {
	Base string
	HTTP *http.Client
	// RequestTimeout 是非流式端点（登录/余额/目录等）的整体超时。
	RequestTimeout time.Duration
	// IdleTimeout 对对话流生效：多久没收到上游数据算超时。
	// 不能用 http.Client.Timeout 管流式——那是整条请求（含读响应体）的总时限，
	// 长回答会被硬掐在超时点，报 "context deadline exceeded while reading body"。
	IdleTimeout time.Duration
}

func NewClient(base string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 10 * time.Second
	// 等响应头的时间上限；拿到响应头后响应体读取由 IdleTimeout 逐次判定
	tr.ResponseHeaderTimeout = timeout
	tr.IdleConnTimeout = 90 * time.Second
	return &Client{
		Base:           strings.TrimRight(base, "/"),
		HTTP:           &http.Client{Transport: tr},
		RequestTimeout: timeout,
		IdleTimeout:    timeout,
	}
}

// Envelope 是小浣熊统一的 {code, message, data} 信封。
//
// 错误体有两种下发形态，必须都认：
//   - 顶层 {"code":1000007,"message":"积分余额不足…"}
//   - OpenAI 形态的嵌套 {"error":{"code":1000007,"message":"…","type":…}}
//
// 后者在 400/402 的业务错误里非常常见。只解析顶层字段时 ErrMessage 返回空，
// 调用方会退化成「把整段原始 JSON 当文案」，面板与流水里就是一大坨转义 JSON。
type Envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
	Error   *EnvelopeError  `json:"error"`
	// body 是原始响应体，供没有 data 字段的端点（模型目录历史版本）解析。
	body []byte
}

// EnvelopeError 是嵌套 error 对象的字段子集。
type EnvelopeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}

// rawBody 返回原始响应体（未解析的字节）。错误体里可能带**文本**业务码
// （`"insufficient_points"`），只靠 Envelope.Code 拿不到。
func (e *Envelope) rawBody() []byte {
	if e == nil {
		return nil
	}
	return e.body
}

func (e *Envelope) ErrMessage() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Msg != "" {
		return e.Msg
	}
	if e.Error != nil {
		return e.Error.Message
	}
	return ""
}

func (e *Envelope) OK() bool { return e.Code == 0 || e.Code == 200 }

func (c *Client) url(path string) string { return c.Base + path }

func (c *Client) doJSON(ctx context.Context, method, path string, headers map[string]string, payload any) (int, *Envelope, error) {
	// 非流式端点用总超时兜底，避免连接卡死时 goroutine 一直挂着
	if c.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.RequestTimeout)
		defer cancel()
	}
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
	PathBalance     = "/api/web/points/v1/balance" // 已下线：上游对任何凭证都回 403，仅作最后兜底
	PathSetting     = "/api/web/office/v3/setting_info"
	PathSettingV2   = "/api/web/office/v2/setting_info"
	PathSettingOrg  = "/api/web/org/user/office/v1/setting_info"
	PathGrant       = "/api/web/desktop/v1/login/points/grant"
	PathMobileGrant = "/api/web/mobile/v1/login/points/grant"
	// PathBills 是积分账单，签到用它核对「今日到底有没有入账」：
	// 发放接口回 200 不等于真发放了，账单才是权威结果。
	PathBills = "/api/web/points/v2/bills"
	// PathIamConfirm 是 IAM 首次迁移的一次性确认（业务码 200901 触发）。
	PathIamConfirm = "/api/web/auth/v1/iam/confirm"
	PathRefresh    = "/api/web/auth/v1/refresh"
	PathUserInfo   = "/api/web/auth/v1/user_info"
	PathExchange   = "/api/web/auth/v1/login_with_authorization_code"
	PathBindDevice = "/api/web/auth/v1/devices_current_bind"
)

// ---- 结果类型 ----

type Model struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ContextLength   int    `json:"contextLength"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
	SupportsImages  bool   `json:"supportsImages"`
	SupportsTools   bool   `json:"supportsTools"`
	SupportsReason  bool   `json:"supportsReasoning"`
	// Hidden 表示上游目录把这条标成 visible:false（官方客户端不列它）。
	//
	// 保留而不是丢弃：这些模型（实测是 raccoon-* 内部 Work 模型）仍需能被
	// 模型管理里的显式映射指向——丢弃会让它们永远进不了清单，也就无法给它们
	// 建别名。对外广告时由转发层按这个标记跳过。
	Hidden bool `json:"hidden,omitempty"`
	// PointsMultiplier 是上游目录的计费倍率（实付优先，缺失时回落基准）。
	// 指针形态：nil = 上游没给，0 = 限免（真实存在的合法值）。
	PointsMultiplier *float64 `json:"pointsMultiplier,omitempty"`
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
		// 刷新端点回 401 = refresh_token 本身不可用，这才是真正需要重新登录的信号。
		return nil, &UpstreamError{Status: status, Code: codeOf(env), Auth: true, Message: "刷新被拒（401），refresh_token 已失效"}
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

// Balance 查账号可用积分。
//
// 通道全部走 setting_info 的 available_points，按存活情况排列：
//
//	v2  ← 当前可用（2026-11-21 实测，无 Cookie 机头也 200）
//	v3  ← 2026-11 起网关整体拦截，对任何凭证都 403（X-TT-System-Error: 3）；
//	      留作第一回退，上游恢复后会自动重新优先
//	旧 /points/v1/balance ← 已下线，只作最后兜底
//
// 团队号先走组织路径，失败再退个人路径；退个人路径时摘掉 X-Org-Code，
// 否则读到的仍是组织口径的积分。
func (c *Client) Balance(ctx context.Context, token string, idn Identity) (int64, error) {
	var lastErr error
	paths := []string{PathSettingV2, PathSetting}
	if isTeam(idn.OfficeIdentity) {
		paths = []string{PathSettingOrg, PathSettingV2, PathSetting}
	}
	for i, path := range paths {
		// 组织路径之后的都按个人口径查
		personal := i > 0
		points, err := c.balanceFromSetting(ctx, token, idn, path, personal)
		if err == nil {
			return points, nil
		}
		lastErr = err
		if isAuthErr(err) {
			// 凭证本身不认，换路径也没用，别让别人以为是在试错
			return 0, err
		}
	}
	if points, err := c.balanceFromLegacy(ctx, token, idn); err == nil {
		return points, nil
	}
	return 0, lastErr
}

// balanceFromSetting 从 setting_info 的 available_points 取积分。
func (c *Client) balanceFromSetting(ctx context.Context, token string, idn Identity, path string, personal bool) (int64, error) {
	headers := webHeaders(token, idn, "web")
	if personal {
		delete(headers, "X-Org-Code")
	}
	status, env, err := c.doJSON(ctx, http.MethodGet, path, headers, nil)
	if err != nil {
		return 0, err
	}
	if status == http.StatusUnauthorized {
		return 0, &UpstreamError{Status: status, Code: codeOf(env), Auth: true, Message: "凭证失效"}
	}
	code, codeName := codeFromBodyEx(env.rawBody())
	if IsAuthCode(code, codeName) || LooksLikeAuthMessage(errText(env, 0)) {
		return 0, &UpstreamError{Status: status, Code: code, CodeName: codeName, Auth: true, Message: "凭证失效"}
	}
	if status < 200 || status >= 300 || env == nil || !env.OK() {
		return 0, &UpstreamError{Status: status, Code: code, CodeName: codeName, Message: "额度查询失败：" + errText(env, status)}
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
	// 字段缺失按「查不到」处理而不是 0：写库会把旧余额覆盖成 0，面板看起来像没钱了
	return 0, errors.New("setting_info 未返回 available_points")
}

// balanceFromLegacy 是旧余额端点的最后兜底，失败结果不向上暴露具体原因。
func (c *Client) balanceFromLegacy(ctx context.Context, token string, idn Identity) (int64, error) {
	status, env, err := c.doJSON(ctx, http.MethodGet, PathBalance, webHeaders(token, idn, ""), nil)
	if err != nil {
		return 0, err
	}
	if status < 200 || status >= 300 || env == nil || !env.OK() {
		return 0, &UpstreamError{Status: status, Code: codeOf(env), Message: errText(env, status)}
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
	return 0, errors.New("旧余额端点未返回积分")
}

func isAuthErr(err error) bool {
	var ue *UpstreamError
	return errors.As(err, &ue) && ue.Auth
}

// Catalog 拉官方模型目录。
func (c *Client) Catalog(ctx context.Context, token string, idn Identity) ([]Model, error) {
	status, env, err := c.doJSON(ctx, http.MethodGet, PathCatalog, webHeaders(token, idn, ""), nil)
	if err != nil {
		return nil, err
	}
	// 上游有时回 401、有时回 403。这里只是拉目录，失败就换下一个号，
	// 不打 Auth 标记——那个标记会被上层解读成「账号要重新登录」。
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, &UpstreamError{Status: status, Message: fmt.Sprintf("模型目录鉴权失败（HTTP %d）", status)}
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
		Name            string `json:"name"`
		ID              string `json:"id"`
		Model           string `json:"model"`
		ModelName       string `json:"model_name"`
		DisplayName     string `json:"display_name"`
		Description     string `json:"description"`
		Visible         *bool  `json:"visible"`
		ContextWindow   int    `json:"context_window"`
		MaxTokens       int    `json:"max_tokens"`
		SupportsImages  *bool  `json:"supports_images"`
		SupportImage    *bool  `json:"supportImage"`
		SupportsReason  *bool  `json:"supports_reasoning"`
		SupportThinking *bool  `json:"supportThinking"`
		// 上游已把 points_multiplier 废弃，实际给的是 billing_multiplier；
		// billing_effective_multiplier 是计入活动与限免后的实付倍率。
		BillingBase      *float64 `json:"billing_multiplier"`
		BillingEffective *float64 `json:"billing_effective_multiplier"`
		PointsMultiplier *float64 `json:"points_multiplier"`
		Params           struct {
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
			reasoning := false
			if it.SupportsReason != nil {
				reasoning = *it.SupportsReason
			} else if it.SupportThinking != nil {
				reasoning = *it.SupportThinking
			} else {
				// 目录没声明时按已确认的模型家族判断（与思考参数改写同一张表）
				reasoning = reasoningSupported(id)
			}
			display := firstNonEmpty(it.DisplayName, shortDesc(it.Description), id)
			hidden := it.Visible != nil && !*it.Visible
			out = append(out, Model{
				ID:               id,
				Name:             display,
				ContextLength:    ctxLen,
				MaxOutputTokens:  maxOut,
				SupportsImages:   imgs,
				SupportsReason:   reasoning,
				Hidden:           hidden,
				PointsMultiplier: billedMultiplier(it.BillingEffective, it.BillingBase, it.PointsMultiplier),
			})
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

// billedMultiplier 取目录里的计费倍率：实付优先，缺失回落基准。
//
// 不用 points_multiplier：上游已废弃该字段（实测当前目录里一个都没有），
// 继续读它只会让倍率恒为「未声明」。0 是合法值（限免），只有 nil 才表示
// 上游没给。
func billedMultiplier(effective, base, legacy *float64) *float64 {
	for _, v := range []*float64{effective, base, legacy} {
		if v != nil && *v >= 0 {
			value := *v
			return &value
		}
	}
	return nil
}

// reasoningSupported 按已确认的模型家族判断是否支持思考（与 reasoning.go 同一张表）。
func reasoningSupported(model string) bool {
	switch familyOf(model) {
	case famThinking, famLowHigh, famEffort:
		return true
	}
	return false
}

// CheckinResult 是签到两步的结论。Already=true 表示今日已领（幂等，不是失败）。
type CheckinResult struct {
	OK      bool
	Claimed bool
	Already bool
	Message string
	// Credit 是账单核对出的**今日实际入账**积分（核对不到为 0）。
	// 发放接口回 200 不等于真发了，只有账单里看得到才算数。
	Credit int64
	// Verified 表示账单核对成功（无论入账是否为 0）。
	// false = 账单接口失败，此时 Credit=0 不代表「没发」，面板不该据此断言。
	Verified bool
}

// Checkin 是每日签到：先由 setting_info 触发发放，再查账单核对**是否真的入账**。
//
// ── 为什么以账单为准 ────────────────────────────────────────
// setting_info 是每日积分的发放触发器（按天幂等），发放通知在
// point_grant_popups / point_grant_toast 里。但它回 200 只代表请求成功，
// 不代表今天真的发了——上游对已领过的号同样回 200。只有积分账单里能看到
// 「今日入账 > 0」才算领到了。旧实现只看 grant 端点，把一次性首登奖的
// granted 当成每日签到结果，账单入账则从未核对过。
//
// ── 失败口径 ────────────────────────────────────────────────
// setting_info 失败即当日没有发放依据，直接如实报失败（旧实现静默吞掉
// 这一步，面板显示「签到成功」但余额没动）。账单核对失败不算失败，但
// Verified=false，调用方不该据 Credit=0 断言「没发」。
func (c *Client) Checkin(ctx context.Context, token string, idn Identity) (*CheckinResult, error) {
	// 第 1 步：发放触发器。失败即当日没有发放依据，如实报失败。
	//
	// 通道顺序与余额查询一致（组织号 → v3 → v2）：v3 在一段时间里对所有凭证
	// 回 403（见 Balance 的注释），而它正是每日发放的触发器——只写死 v3
	// 会让签到每天都在第一步静默失败，账单核对也拿不到入账。
	status, env, err := c.settingInfo(ctx, token, idn)
	if err != nil {
		return nil, err
	}
	// 凭证失效不只看 401：上游也会把 200003/200009（或文本码
	// authorization_verify_error）挂在 4xx/200 的信封里，只认状态码会把它
	// 当成「发放失败」，于是这个号既不冷却也不提示重登。
	code, codeName := codeFromBodyEx(env.rawBody())
	if status == http.StatusUnauthorized || IsAuthCode(code, codeName) || LooksLikeAuthMessage(errText(env, 0)) {
		return nil, &UpstreamError{Status: status, Code: code, CodeName: codeName, Auth: true, Message: "凭证失效，请重新登录"}
	}
	if status < 200 || status >= 300 || env == nil || !env.OK() {
		return &CheckinResult{OK: false, Message: "每日积分发放失败：" + errText(env, status)}, nil
	}

	// 第 2 步：账单核对今日入账（权威结果）。核对失败不阻断签到本身。
	today := beijingToday()
	grants, verifyErr := c.dailyGrants(ctx, token, idn, today)
	credit := int64(0)
	for _, g := range grants {
		credit += g.Points
	}
	verified := verifyErr == nil

	res := &CheckinResult{OK: true, Verified: verified, Credit: credit}
	switch {
	case credit > 0:
		res.Claimed = true
		res.Message = fmt.Sprintf("今日积分 +%d", credit)
	case !verified:
		res.Message = "已触发发放，但账单核对失败：" + verifyErr.Error()
	default:
		res.Already = true
		res.Message = "今日未见积分入账（可能此前已领取）"
	}
	return res, nil
}

// settingInfo 按「组织路径 → v3 → v2」依次尝试发放触发器，返回第一个成功的结果。
//
// 与 Balance 的通道顺序同一取向：v3 在上游被网关拦截过一段时间（对任何凭证
// 都回 403），v2 一直是可用的那条。凭证失效（401/Auth 码）时不换路径——
// 换一条也是同样的 401，只会让日志更难读。
//
// 全部失败时返回最后一次的响应，由调用方按状态码与错误码决定对外文案。
func (c *Client) settingInfo(ctx context.Context, token string, idn Identity) (int, *Envelope, error) {
	paths := []string{PathSetting, PathSettingV2}
	if isTeam(idn.OfficeIdentity) {
		paths = append([]string{PathSettingOrg}, paths...)
	}
	var (
		lastStatus int
		lastEnv    *Envelope
	)
	for _, path := range paths {
		headers := webHeaders(token, idn, "web")
		// 组织路径之后的个人路径要摘掉 X-Org-Code，否则读到的仍是组织口径。
		if path != PathSettingOrg || !isTeam(idn.OfficeIdentity) {
			delete(headers, "X-Org-Code")
		}
		status, env, err := c.doJSON(ctx, http.MethodGet, path, headers, nil)
		if err != nil {
			return 0, nil, err
		}
		code, codeName := codeFromBodyEx(env.rawBody())
		if status == http.StatusUnauthorized || IsAuthCode(code, codeName) || LooksLikeAuthMessage(errText(env, 0)) {
			// 凭证问题：换路径没用，直接把这一跳的结果交给调用方。
			return status, env, nil
		}
		if status >= 200 && status < 300 && env != nil && env.OK() {
			return status, env, nil
		}
		lastStatus, lastEnv = status, env
	}
	return lastStatus, lastEnv, nil
}

// 账单核对参数：拉取条数与超时。
//
// 账单接口很慢（实测 limit=20 约 9 秒），用独立更宽的超时兜底，避免它
// 把签到整体拖过上游超时；翻页最多 5 页，遇到昨天的账单就停。
const (
	checkBillLimit   = 20
	checkBillPages   = 5
	checkBillTimeout = 25 * time.Second
)

// DailyGrant 是账单里的一条今日入账。
type DailyGrant struct {
	Name   string
	Points int64
	At     string
}

// dailyGrants 拉今日的积分入账（北京时间自然日）。
//
// 返回的 error 只表示「核对失败」（网络/非 2xx/结构异常），与「今日没有入账」
// 区分开：前者 Verified=false，后者 Verified=true 且列表为空。
func (c *Client) dailyGrants(ctx context.Context, token string, idn Identity, today string) ([]DailyGrant, error) {
	headers := webHeaders(token, idn, "")
	headers["accept"] = "application/json"
	var out []DailyGrant
	cursor := ""
	seen := map[string]bool{}
	deadline := time.Now().Add(checkBillTimeout)
	for pageNo := 0; pageNo < checkBillPages; pageNo++ {
		if time.Now().After(deadline) {
			break
		}
		query := fmt.Sprintf("?paging.limit=%d&type=all", checkBillLimit)
		if cursor != "" {
			query += "&cursor=" + urlQueryEscape(cursor)
		}
		status, env, err := c.doJSON(ctx, http.MethodGet, PathBills+query, headers, nil)
		if err != nil {
			return out, err
		}
		code, codeName := codeFromBodyEx(env.rawBody())
		if status == http.StatusUnauthorized || IsAuthCode(code, codeName) {
			return out, &UpstreamError{Status: status, Code: code, CodeName: codeName, Auth: true, Message: "凭证失效"}
		}
		if status < 200 || status >= 300 || env == nil || !env.OK() {
			return out, fmt.Errorf("账单接口返回 HTTP %d：%s", status, errText(env, status))
		}
		var data struct {
			Items   []map[string]any `json:"items"`
			Cursor  string           `json:"next_cursor"`
			Cursor2 string           `json:"cursor"`
			HasMore *bool            `json:"has_more"`
			Paging  map[string]any   `json:"paging"`
		}
		raw := rawData(env)
		if len(raw) == 0 {
			return out, errors.New("账单响应缺少 data")
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return out, fmt.Errorf("账单响应结构异常：%v", err)
		}
		if data.Items == nil {
			return out, errors.New("账单响应缺少 items")
		}
		reachedYesterday := false
		for _, item := range data.Items {
			day := billDay(item)
			if day != "" && day < today {
				reachedYesterday = true
			}
			if g, ok := dailyGrant(item, today); ok {
				out = append(out, g)
			}
		}
		if reachedYesterday || len(data.Items) == 0 {
			break
		}
		next := firstNonEmpty(data.Cursor, data.Cursor2, anyStr(data.Paging["next_cursor"]), anyStr(data.Paging["cursor"]))
		hasMore := true
		if data.HasMore != nil {
			hasMore = *data.HasMore
		}
		if !hasMore || next == "" || seen[next] {
			break
		}
		seen[next] = true
		cursor = next
	}
	return out, nil
}

// billDay 取一条账单的日期键（YYYY-MM-DD，按北京时间）。
func billDay(item map[string]any) string {
	at := firstNonEmpty(anyStr(item["grant_at"]), anyStr(item["created_at"]))
	if at == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return t.In(chinaZone()).Format("2006-01-02")
	}
	if len(at) >= 10 {
		return at[:10]
	}
	return ""
}

// dailyGrant 判断一条账单是否是「今日的每日积分发放」，是则取出金额。
//
// 判定与后台链路一致：biz_type == "daily_grant"，或没有 biz_type 时按
// event_name == "每日积分发放" 兜底。points <= 0 与「不是今天」都不算。
func dailyGrant(item map[string]any, today string) (DailyGrant, bool) {
	kind := anyStr(item["biz_type"])
	name := anyStr(item["event_name"])
	isDaily := kind == "daily_grant" || (kind == "" && name == "每日积分发放")
	points := toInt64(item["points"])
	if !isDaily || points <= 0 || billDay(item) != today {
		return DailyGrant{}, false
	}
	at := anyStr(item["created_at"])
	if len(at) >= 19 {
		at = at[11:19]
	}
	return DailyGrant{Name: name, Points: points, At: at}, true
}

// chinaZone 是上游自然日的时区（北京时间 UTC+8）。
//
// 用固定偏移而不是 time.LoadLocation("Asia/Shanghai")：容器镜像里未必带
// tzdata，加载失败会静默回落 UTC，把 16 小时的账单认成昨天。
func chinaZone() *time.Location {
	return time.FixedZone("CST", 8*3600)
}

// beijingToday 返回今天的北京时间日期键（YYYY-MM-DD）。
func beijingToday() string {
	return time.Now().In(chinaZone()).Format("2006-01-02")
}

// urlQueryEscape 对查询参数做最小转义（账单 cursor 是 base64 形态，含 + / =）。
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// toInt64 把数值或数字字符串取整；其它形态给 0。
func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return int64(f)
		}
	}
	return 0
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
//
// IAM 首次迁移要先过一次性确认：业务码 200901 出现时打一次 iam/confirm，
// 再重新拉 user_info。少了这一步，IAM 号的资料查询会一直停在 200901，
// 表现为「导入成功但账号资料是空的」。
func (c *Client) UserInfo(ctx context.Context, token string, idn Identity) (uid, name string) {
	headers := webHeaders(token, idn, "")
	status, env, err := c.doJSON(ctx, http.MethodGet, PathUserInfo, headers, nil)
	if err != nil {
		return TokenUID(token), ""
	}
	if code, _ := codeFromBodyEx(env.rawBody()); code == CodeIamConfirmRequired {
		// 一次性确认：失败不阻断，下一次查询会再走一遍；成功后再拉一次资料。
		if cst, cenv, cerr := c.doJSON(ctx, http.MethodPost, PathIamConfirm, headers, map[string]any{}); cerr == nil &&
			cst >= 200 && cst < 300 && cenv != nil && cenv.OK() {
			status, env, err = c.doJSON(ctx, http.MethodGet, PathUserInfo, headers, nil)
			if err != nil {
				return TokenUID(token), ""
			}
		}
	}
	if status < 200 || status >= 300 || env == nil {
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
	fp := CurrentFingerprint()
	payload := map[string]any{
		"client_device_id_source": "unknown",
		"client_device_id":        deviceID,
		"client_platform":         "desktop-windows",
		"client_version":          fp.ClientVersion,
		"device_name":             name,
		"os":                      "windows",
		"os_version":              fp.OSVersion,
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
	Status int
	// Code 是上游的业务错误码。非数字码（例如 `"insufficient_points"`）保留在
	// CodeName 里：上游按数字码分类，不认识的字符串码至少要让「这是什么错」
	// 还能被文案与日志看见，而不是退化成 0 被当成「没有错误码」。
	Code     int
	CodeName string
	Auth     bool
	Message  string

	// Cause 是底层错误（网络、context 等），只用于错误链判断，不参与对外文案。
	//
	// 必须保留：上游流读到一半被打断时，只有 context.Canceled 这个 sentinel
	// 能区分「请求方已经走了」与「上游真的出错」。丢掉它，转发层就会把一次
	// 用户取消当成上游故障——给健康号记退避、在面板上记成 502。
	Cause error

	// RetryAfter 是上游建议的重试等待时长。来源按优先级：
	// Retry-After 头（秒数或 HTTP 日期）、x-ratelimit-reset 头、
	// 响应体里的 reset_time / retry_after 字段（秒数、秒级或毫秒级时间戳）。
	// 0 表示上游没有给出；转发层据此把冷却对齐到上游真正解除限制的时刻，
	// 而不是一律按本地阶梯等 3 分钟后再撞一次 429。
	RetryAfter time.Duration

	// 以下字段只用于面板的「错误详情」排查，不参与分类、冷却与对外文案。
	// 403 的真实原因几乎都在响应头或响应体里（风控命中、权限不足、网关拦截），
	// 只留一句「HTTP 403」用户无从判断，所以把现场完整带出来。
	Endpoint       string            // 上游路径
	TurnID         string            // 本次请求的 Turn-ID，便于和上游日志对齐
	RequestID      string            // 上游返回的请求 ID（有的话）
	RequestSummary string            // 请求侧摘要：模型、客户端指纹头
	Headers        map[string]string // 脱敏后的响应头
	Raw            string            // 原始响应体（截断）
}

// attachDiagnostics 把一次非 2xx 响应的现场挂在错误上，供运行事件与面板展示。
func (e *UpstreamError) attachDiagnostics(h http.Header, raw []byte) {
	e.Headers = diagnosticHeaders(h)
	e.Raw = truncateRaw(raw)
	e.RequestID = requestIDOf(e.Headers)
}

// secretRespHeaders 是不能出现在面板上的响应头（可能携带凭据）。
var secretRespHeaders = map[string]bool{
	"set-cookie": true, "cookie": true, "authorization": true, "proxy-authorization": true,
}

func diagnosticHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		lk := strings.ToLower(k)
		if secretRespHeaders[lk] {
			continue
		}
		out[lk] = strings.Join(vs, ", ")
	}
	return out
}

// truncateRaw 把原始响应体转成可放进事件详情的文本。按字符截断，
// 避免把多字节汉字切坏；上限 3000 字符足够看清网关拦截页或错误 JSON。
func truncateRaw(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	const max = 3000
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "\n…（已截断，原始响应体更长）"
	}
	return s
}

// requestIDHeaders 是各家网关常用的请求 ID 头，命中哪个就展示哪个。
var requestIDHeaders = []string{
	"x-request-id", "x-trace-id", "x-tt-logid", "x-tt-request-id",
	"request-id", "x-amzn-requestid", "traceparent", "cf-ray",
}

func requestIDOf(h map[string]string) string {
	for _, k := range requestIDHeaders {
		if v := h[k]; v != "" {
			return v
		}
	}
	return ""
}

func (e *UpstreamError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("上游错误 HTTP %d", e.Status)
}

// Unwrap 暴露底层错误，让 errors.Is(err, context.Canceled) 这类判断能穿过
// 包装层。只用于分类，对外文案仍走 Message。
func (e *UpstreamError) Unwrap() error { return e.Cause }

func codeOf(env *Envelope) int {
	if env == nil {
		return 0
	}
	if env.Code != 0 {
		return env.Code
	}
	if env.Error != nil {
		return env.Error.Code
	}
	return 0
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
