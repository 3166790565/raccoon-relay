// Package autoclaw 实现智谱 AutoClaw（autoglm）的账号接入与对话转发。
//
// 协议全部取自 magpie 社区插件 @magpie-community/opencode-autoclaw-auth，
// 与上游桌面客户端 AutoClaw 的实际行为对齐。要点：
//
//   - 两家站点：国际站 autoglm-api.autoglm.ai（网页 OAuth），
//     国内站 autoglm-api.zhipuai.cn（手机验证码）。同一个账号只属于一个站点。
//   - 对话鉴权放在 X-Authorization，而不是 Authorization；
//     业务接口（余额/签到/刷新）才用 Authorization。
//   - 业务请求带 X-Auth-Appid / X-Auth-TimeStamp / X-Auth-Sign（MD5 签名）。
//   - 对话网关对请求形状有硬性要求：system 必须以官方前缀开头、
//     不能带 max_tokens、不能带 X-Harness-Type，否则一律 406。
//   - refresh_token 一次性，轮换后旧值即作废，必须串行续期并整体写回。
package autoclaw

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// 上游固定地址与客户端标识（与插件逐项对齐）。
const (
	AppID     = "100003"
	AppKey    = "38d2391985e2369a5fb8227d8e6cd5e5"
	Product   = "autoclaw"
	Channel   = "AutoClaw4"
	ClientVer = "1.18.1"

	GlobalHost = "https://autoglm-api.autoglm.ai"
	CNHost     = "https://autoglm-api.zhipuai.cn"
	// CNManagedHost 是国内站的对话加速网关。
	CNManagedHost = "https://autoglm-acceleration-api.zhipuai.cn"

	// 国际站对话路径带 v1，国内加速网关不带。
	chatPathGlobal = "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	chatPathCN     = "/autoclaw-proxy/proxy/autoclaw/chat/completions"

	pathWallets  = "/agent-assetmgr/api/v2/wallets?biz_app_id=" + Product
	pathRefresh  = "/userapi/v1/refresh"
	pathAgentRef = "/userapi/v1/agent-refresh"
	pathTaskList = "/autoclaw-proxy/proxy/autoclaw-task-list"
	pathTaskDone = "/autoclaw-proxy/proxy/autoclaw-task-complete"
	pathUserInfo = "/userapi/v1/user-info"
	pathUserProf = "/userapi/v1/user-profile"

	// OfficialSystemPrompt 是对话网关强制的 system 前缀，缺了回 406。
	OfficialSystemPrompt = "You are AutoClaw. Answer the user directly and concisely."

	// 国际站对话走官方 2.0.3 桌面端指纹。
	officialChannel = "official"
	officialVersion = "2.0.3"
	openaiSDKVer    = "6.26.0"
	officialNodeVer = "v24.18.0"

	// CNChannel / CNClientVer 是国内站对话网关认的渠道与版本。
	CNChannel   = "AutoClaw26"
	CNClientVer = "2.1.0"

	// expirySkew 在计入的到期时刻上再提前 10 分钟：上游有时不给 expires_in，
	// 只给一个带 exp 的 JWT；提前量留出写回与网络抖动的时间。
	expirySkew = 10 * time.Minute
)

// Client 是 AutoClaw 上游 HTTP 客户端。
type Client struct {
	HTTP           *http.Client
	GlobalHost     string
	CNHost         string
	CNManagedHost  string
	RequestTimeout time.Duration
	// IdleTimeout 约束对话流的空闲时间。
	IdleTimeout time.Duration
}

// NewClient 构造默认客户端。
func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = timeout
	tr.IdleConnTimeout = 90 * time.Second
	return &Client{
		HTTP:           &http.Client{Transport: tr},
		GlobalHost:     GlobalHost,
		CNHost:         CNHost,
		CNManagedHost:  CNManagedHost,
		RequestTimeout: timeout,
		IdleTimeout:    timeout,
	}
}

// hostFor 返回账号站点对应的业务接口 host。
func (c *Client) hostFor(acc *store.Account) string {
	if acc.Station() == store.StationCN {
		return strings.TrimRight(c.CNHost, "/")
	}
	return strings.TrimRight(c.GlobalHost, "/")
}

// chatURLFor 返回账号站点的对话地址。
func (c *Client) chatURLFor(acc *store.Account) string {
	if acc.Station() == store.StationCN {
		return strings.TrimRight(c.CNManagedHost, "/") + chatPathCN
	}
	return strings.TrimRight(c.GlobalHost, "/") + chatPathGlobal
}

// ---------- 凭据 ----------

// Credential 是 AutoClaw 账号的完整凭据，序列化后存在账号的 ExtExtra 里；
// ExtToken / ExtRefreshToken / ExtTokenExp 三列是同一份数据的「便于查询」副本，
// 选号与到期判断只读那三列，不必每次解 JSON。
type Credential struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	// Expires 是 access token 的到期时刻（毫秒），已扣除 expirySkew。
	Expires  int64  `json:"expires"`
	UserID   string `json:"uid,omitempty"`
	Name     string `json:"name,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	// Source 记录来源（paste / agent2api / cn-sms / oauth），便于排查。
	Source string `json:"source,omitempty"`
}

// DecodeCred 从账号上还原凭据。ExtExtra 是权威副本；为空时退回
// ExtToken/ExtRefreshToken/ExtTokenExp 三列（手工导入的最小形态）。
func DecodeCred(acc *store.Account) (Credential, error) {
	var c Credential
	if strings.TrimSpace(acc.ExtExtra) != "" {
		if err := json.Unmarshal([]byte(acc.ExtExtra), &c); err != nil {
			return c, fmt.Errorf("AutoClaw 凭据解析失败: %w", err)
		}
	}
	if c.Access == "" {
		c.Access = acc.ExtToken
	}
	if c.Refresh == "" {
		c.Refresh = acc.ExtRefreshToken
	}
	if c.Expires == 0 {
		c.Expires = acc.ExtTokenExp
	}
	if c.Access == "" && c.Refresh == "" {
		return c, errors.New("AutoClaw 账号没有可用凭据")
	}
	return c, nil
}

// Patch 把凭据回写成账号补丁。site 只在需要改动站点时传非空；
// 传空串表示保持账号现有站点不变。
func (c Credential) Patch(site string) store.AccountPatch {
	raw, _ := json.Marshal(c)
	p := store.AccountPatch{
		ExtToken:        strPtr(c.Access),
		ExtTokenExp:     int64Ptr(c.Expires),
		ExtRefreshToken: strPtr(c.Refresh),
		ExtExtra:        strPtr(string(raw)),
	}
	if site != "" {
		p.ExtSite = strPtr(site)
	}
	return p
}

// tokenFields 把上游的 token 响应归一成凭据。
func tokenFields(data map[string]any) Credential {
	var c Credential
	c.Access = bareToken(strOf(data["access_token"]))
	c.Refresh = bareToken(strOf(data["refresh_token"]))
	if c.Access != "" {
		if exp := jwtExpMillis(c.Access); exp > 0 {
			c.Expires = exp - expirySkew.Milliseconds()
		}
	}
	if c.Expires == 0 {
		if secs := numOf(data["expires_in"]); secs > 0 {
			c.Expires = time.Now().Add(time.Duration(secs)*time.Second).UnixMilli() - expirySkew.Milliseconds()
		}
	}
	return c
}

// jwtExpMillis 读 JWT 的 exp（秒）并转成毫秒；解不出来返回 0。
// 只用于本地到期判断，不校验签名。
func jwtExpMillis(token string) int64 {
	claims := jwtPayload(token)
	if claims == nil {
		return 0
	}
	if exp, ok := claims["exp"].(float64); ok && exp > 0 {
		return int64(exp) * 1000
	}
	return 0
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := b64urlDecode(parts[1])
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// jwtClaim 读一个字符串 claim。
func jwtClaim(token, key string) string {
	claims := jwtPayload(token)
	if claims == nil {
		return ""
	}
	return strOf(claims[key])
}

// bareToken 去掉上游偶发的 "Bearer " 前缀，保证存进库的是裸 token。
func bareToken(t string) string {
	t = strings.TrimSpace(t)
	for strings.HasPrefix(strings.ToLower(t), "bearer ") {
		t = strings.TrimSpace(t[7:])
	}
	return t
}

// ---------- 业务请求 ----------

// businessHeaders 是余额/签到/刷新等业务接口的头集，与插件 bizHeaders 一致。
// accessToken 为空时不带 Authorization（登录/刷新就是这样）。
func (c *Client) businessHeaders(accessToken string) map[string]string {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	h := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "*/*",
		"User-Agent":       fmt.Sprintf("AutoClaw/%s (%s-%s)", ClientVer, platformTm(), archName()),
		"X-Version":        ClientVer,
		"X-Tm":             platformTm(),
		"X-Product":        Product,
		"X-Auth-Appid":     AppID,
		"X-Auth-TimeStamp": ts,
		"X-Auth-Sign":      authSign(ts),
		"X-Trace-Id":       randomHex(16),
		"X-Lang":           "zh-CN",
		"X-Channel":        Channel,
		"Origin":           "http://localhost:18432",
		"Referer":          "http://localhost:18432/",
	}
	if accessToken != "" {
		h["Authorization"] = "Bearer " + accessToken
	}
	return h
}

// authSign 是业务接口的 MD5 签名：md5("<appid>&<秒级时间戳>&<appkey>")。
func authSign(ts string) string {
	sum := md5.Sum([]byte(AppID + "&" + ts + "&" + AppKey))
	return hex.EncodeToString(sum[:])
}

// envelope 是业务接口统一的 {code, msg, data} 信封。
type envelope struct {
	Code    json.RawMessage `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *envelope) code() int    { return int(numOf(jsonValue(e.Code))) }
func (e *envelope) text() string { return firstNonEmpty(e.Msg, e.Message) }

// doBusiness 发一次业务请求并解开信封。非 2xx 折成 UpstreamError。
func (c *Client) doBusiness(ctx context.Context, host, method, path, token string, body any) (*envelope, error) {
	var reader io.Reader
	switch {
	case body != nil:
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(raw))
	case method != http.MethodGet:
		reader = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, method, host+path, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range c.businessHeaders(token) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: c.HTTP.Transport, Timeout: c.RequestTimeout}
	res, err := client.Do(req)
	if err != nil {
		return nil, &raccoon.UpstreamError{Status: 0, Message: "AutoClaw 上游连接失败: " + err.Error(), Endpoint: path}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	var env envelope
	_ = json.Unmarshal(raw, &env)
	if res.StatusCode >= 400 {
		return &env, classifyError(res.StatusCode, res.Header, raw, path, false)
	}
	return &env, nil
}

// classifyError 把非 2xx 折成 UpstreamError。
//
// 401/403 要区分三种情况：额度用尽（429，换号继续）、封号（Auth，需人工）、
// 登录失效（Auth）。文案与 code 都要看——上游对这三种情况有时用一个状态码。
func classifyError(status int, header http.Header, body []byte, endpoint string, chat bool) *raccoon.UpstreamError {
	var env envelope
	_ = json.Unmarshal(body, &env)
	msg := firstNonEmpty(env.text(), strings.TrimSpace(string(body)))
	code := env.code()
	ue := &raccoon.UpstreamError{Status: status, Message: shorten(msg, 500), Endpoint: endpoint}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		switch {
		case isQuotaText(msg) || code == 810000:
			ue.Status = http.StatusTooManyRequests
			ue.Message = "AutoClaw 额度已用尽：" + ue.Message
		case isBannedText(msg) || code == 410004:
			// 封号是账号级永久问题，让转发层把号标成需重登、不再空转。
			ue.Auth = true
			ue.Message = "AutoClaw 账号被封禁：" + ue.Message
		default:
			ue.Auth = true
		}
	case status == http.StatusTooManyRequests:
		ue.RetryAfter = raccoon.RetryAfter(header, body)
	case status == http.StatusNotAcceptable && chat:
		ue.Message = firstNonEmpty(ue.Message, "AutoClaw 拒绝了这次请求的 system 内容")
	}
	if ue.Message == "" {
		ue.Message = fmt.Sprintf("AutoClaw 上游 HTTP %d", status)
	}
	return ue
}

var (
	quotaRe  = regexp.MustCompile(`(?i)额度|余额不足|用完|用尽|quota|insufficient[_ ]?(balance|credit)|exhausted`)
	bannedRe = regexp.MustCompile(`(?i)banned|封禁|封号|账号异常|账户异常|account (has been|is|was )?(suspended|frozen|disabled|blocked|restricted)|账[号户].{0,6}(冻结|停用|禁用)`)
	// deadSignRe 判定「登录态真的失效」。只有 refresh 时用它下最终结论，
	// 对话路径上的 401/403 交给转发层按 Auth 分类。
	deadSignRe = regexp.MustCompile(`(?i)invalid[_ ]?grant|revoked|expired|登录已失效|token 失效|登录状态失效`)
)

func isQuotaText(s string) bool  { return quotaRe.MatchString(s) }
func isBannedText(s string) bool { return bannedRe.MatchString(s) }
func isDeadSignText(s string) bool {
	return deadSignRe.MatchString(s)
}

// ---------- 续期 ----------

// refreshMu 保证同一账号的续期串行：refresh_token 一次性，并发刷新会让
// 后刷的把先刷的轮换结果顶掉，账号直接掉登录。
var refreshMu sync.Map // accountID -> *sync.Mutex

// LockForAccount 返回该账号的进程内刷新锁。调用方应在续期前后自行加锁；
// 多副本之间还要靠库里的 ext_rev CAS 兜底（见 store.ReplaceExtCredential）。
func LockForAccount(id string) *sync.Mutex {
	v, _ := refreshMu.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Refresh 用 refresh_token 换新凭据。调用方应已持有该账号的刷新锁。
//
// 站点优先按 refresh_token 自带的 source_id claim 判断：国内短信登录的号
// 在 claim 里带 agent 前缀，走国内站。这样即便账号的 ExtSite 还没写对，
// 也不会把国内号打到国际网关上去。
func (c *Client) Refresh(ctx context.Context, cred Credential, site string) (Credential, error) {
	rt := bareToken(cred.Refresh)
	if rt == "" {
		return cred, &raccoon.UpstreamError{Status: 401, Auth: true, Message: "AutoClaw 没有 refresh_token，请重新导入凭据"}
	}
	deviceID := jwtClaim(rt, "device_id")
	if deviceID == "" {
		deviceID = cred.DeviceID
	}
	body := map[string]any{
		"source_id":     Product,
		"device_id":     deviceID,
		"refresh_token": rt,
	}
	if strings.HasPrefix(strings.ToLower(jwtClaim(rt, "source_id")), "agent") {
		site = store.StationCN
	}

	var lastErr error
	for _, host := range c.refreshHosts(site) {
		env, err := c.doBusiness(ctx, host, http.MethodPost, pathRefresh, "", body)
		if err == nil && env.code() == 400002 {
			// 国内号要打 agent-refresh 端点。
			env, err = c.doBusiness(ctx, host, http.MethodPost, pathAgentRef, "", body)
		}
		if err == nil && env.code() == 400000 {
			// 该 host 繁忙，换下一个。
			lastErr = &raccoon.UpstreamError{Status: 0, Message: "AutoClaw 刷新接口繁忙"}
			continue
		}
		if err == nil && env.code() != 0 {
			err = &raccoon.UpstreamError{Status: 400, Message: "AutoClaw 刷新失败：" + firstNonEmpty(env.text(), fmt.Sprintf("code %d", env.code()))}
		}
		if err == nil {
			var data map[string]any
			_ = json.Unmarshal(env.Data, &data)
			next := tokenFields(data)
			if next.Access == "" {
				lastErr = &raccoon.UpstreamError{Status: 0, Message: "AutoClaw 刷新响应缺少 access_token"}
				continue
			}
			if next.Refresh == "" {
				next.Refresh = rt
			}
			next.DeviceID, next.UserID, next.Name, next.Source = cred.DeviceID, cred.UserID, cred.Name, cred.Source
			return next, nil
		}
		lastErr = err
		var ue *raccoon.UpstreamError
		if errors.As(err, &ue) && ue.Auth && (isDeadSignText(ue.Message) || ue.Status == http.StatusUnauthorized) {
			// refresh_token 真的失效了：不再换 host 重试。
			ue.Message = "AutoClaw 登录已失效，请重新导入凭据"
			return cred, ue
		}
	}
	if lastErr == nil {
		lastErr = &raccoon.UpstreamError{Status: 0, Message: "AutoClaw 刷新失败（所有站点均无响应）"}
	}
	return cred, lastErr
}

// refreshHosts 给出刷新时的站点尝试顺序：先本号站点，再另一站点兜底。
func (c *Client) refreshHosts(site string) []string {
	cn := strings.TrimRight(c.CNHost, "/")
	global := strings.TrimRight(c.GlobalHost, "/")
	if site == store.StationCN {
		return []string{cn, global}
	}
	return []string{global, cn}
}

// ---------- 对话 ----------

// OpenChat 发起一次对话。返回与其它上游同形的 ChatStream。
func (c *Client) OpenChat(ctx context.Context, acc *store.Account, body map[string]any) (*raccoon.ChatStream, error) {
	cred, err := DecodeCred(acc)
	if err != nil {
		return nil, &raccoon.UpstreamError{Status: 401, Auth: true, Message: err.Error()}
	}
	if cred.Access == "" {
		return nil, &raccoon.UpstreamError{Status: 401, Auth: true, Message: "AutoClaw 凭据缺少 access_token"}
	}

	reqBody := RewriteBody(body, acc.Station())
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	model, _ := reqBody["model"].(string)
	url := c.chatURLFor(acc)

	streamCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		cancel()
		return nil, err
	}
	c.chatHeaders(req, acc, cred, model, body)

	res, err := (&http.Client{Transport: c.HTTP.Transport}).Do(req)
	if err != nil {
		cancel()
		return nil, &raccoon.UpstreamError{Status: 0, Message: "AutoClaw 上游连接失败: " + err.Error(), Endpoint: url}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		rawBody, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		cancel()
		return nil, classifyError(res.StatusCode, res.Header, rawBody, url, true)
	}

	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return raccoon.NewChatStream(res, sc, cancel, ctx, c.IdleTimeout), nil
}

// chatHeaders 按站点设置对话请求头。
//
// 国际站复刻官方 2.0.3 桌面端（含 OpenAI SDK 的 X-Stainless-*），国内站
// 复刻 2.1.0 加速网关。鉴权头是 X-Authorization，不是 Authorization；
// X-Harness-Type 绝不能发，发了必 406。
func (c *Client) chatHeaders(req *http.Request, acc *store.Account, cred Credential, model string, body map[string]any) {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	rid := raccoon.NewTurnID()
	if acc.Station() == store.StationCN {
		for k, v := range c.businessHeaders(cred.Access) {
			req.Header.Set(k, v)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", fmt.Sprintf("AutoClaw/%s (%s-%s)", CNClientVer, platformTm(), archName()))
		req.Header.Set("X-Channel", CNChannel)
		req.Header.Set("X-Version", CNClientVer)
		req.Header.Set("X-Trace-Id", randomHex(16))
		req.Header.Set("X-Request-Id", rid)
		req.Header.Set("X-Request-Model", firstNonEmpty(model, "zai_auto"))
		req.Header.Set("X-Client-Type", "pc")
		req.Header.Set("x_trace_id", "autoclaw-desktop")
		req.Header.Set("X-Agent-Id", "main")
		req.Header.Set("X-Authorization", "Bearer "+bareToken(cred.Access))
		req.Header.Del("Authorization")
		req.Header.Del("X-Harness-Type")
		return
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "OpenAI/JS "+openaiSDKVer)
	req.Header.Set("X-Stainless-Retry-Count", "0")
	req.Header.Set("X-Stainless-Timeout", "600")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-Package-Version", openaiSDKVer)
	req.Header.Set("X-Stainless-OS", stainlessOS())
	req.Header.Set("X-Stainless-Arch", stainlessArch())
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Stainless-Runtime-Version", officialNodeVer)
	req.Header.Set("X-Version", officialVersion)
	// 国际网关对 Linux 指纹精确拒收（403 forbidden），Linux 上直接不发该头。
	if platformTm() != "linux" {
		req.Header.Set("X-Tm", platformTm())
	}
	req.Header.Set("X-Product", Product)
	req.Header.Set("X-Auth-Appid", AppID)
	req.Header.Set("X-Auth-TimeStamp", ts)
	req.Header.Set("X-Auth-Sign", authSign(ts))
	req.Header.Set("X-Lang", "en")
	req.Header.Set("X-Channel", officialChannel)
	req.Header.Set("X-Trace-Id", rid)
	req.Header.Set("X-Request-Id", rid)
	if model != "" {
		req.Header.Set("X-Request-Model", model)
	}
	req.Header.Set("X-Client-Type", "pc")
	req.Header.Set("x_trace_id", "autoclaw-desktop")
	req.Header.Set("X-Session-Id", SessionID(body))
	req.Header.Del("Authorization")
	req.Header.Del("X-Harness-Type")
	req.Header.Set("X-Authorization", "Bearer "+bareToken(cred.Access))
}

func stainlessOS() string {
	switch platformTm() {
	case "mac":
		return "MacOS"
	case "win":
		return "Windows"
	}
	return "Linux"
}

func stainlessArch() string {
	switch archName() {
	case "x64":
		return "x64"
	case "arm64":
		return "arm64"
	}
	return "arm"
}
