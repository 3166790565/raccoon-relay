// Package codearts 实现华为云码道（CodeArts Agent）的账号接入与对话转发。
//
// 协议取自 magpie 社区插件 @magpie-community/opencode-codearts-auth，
// 并与 AgentHub / Agent2API 里的历史实现交叉校验。要点：
//
//   - 两个站点：国内站 snap-access.cn-north-4.myhuaweicloud.com（另有 opengw
//     网关负责免费额度与每日领取），国际站 snap-access.ap-southeast-1。
//   - 不是 API Key 鉴权，而是把每次请求用临时 AK/SK/security-token 做
//     SDK-HMAC-SHA256 签名（见 signer.go）。
//   - 临时凭据由华为云 STS 用 OAuth（PKCE + DPoP）换取，refresh_token 一次性。
//   - 对话只对「流式」计量：请求一律以 stream=true 发出，非流式由转发层折叠。
//   - 网关每账号最多 3 个并发会话，按 x-ot-session-id 识别。
package codearts

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// 站点与固定地址。
const (
	MainlandSnap    = "https://snap-access.cn-north-4.myhuaweicloud.com"
	MainlandOpengw  = "https://opengw.developer.huaweicloud.com"
	InternationalSn = "https://snap-access.ap-southeast-1.myhuaweicloud.com"
	STSEndpoint     = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	IAMHost         = "https://iam.cn-north-4.myhuaweicloud.com"

	ClientID       = "codearts-agent"
	LoginPlugin    = "snap_AIIDE"
	LoginPluginVer = "5.2.0"

	// 单账号最多 3 个并发会话（上游硬限制）。
	MaxSessionsPerAccount = 3
)

// chatPath 是对话路径。
const chatPath = "/api/v2/chat/completions"

// stationHosts 返回某站点的 snap 与 opengw 地址。
func stationHosts(site string) (snap, opengw string) {
	if site == store.StationGlobal {
		return InternationalSn, ""
	}
	return MainlandSnap, MainlandOpengw
}

// Client 是 CodeArts 上游 HTTP 客户端。
type Client struct {
	HTTP           *http.Client
	RequestTimeout time.Duration
	IdleTimeout    time.Duration

	// 站点基址，测试时可覆盖。
	SnapMainland   string
	SnapIntl       string
	OpengwMainland string
	STS            string
	IAM            string
}

func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = timeout
	tr.IdleConnTimeout = 90 * time.Second
	return &Client{
		HTTP:           &http.Client{Transport: tr},
		RequestTimeout: timeout,
		IdleTimeout:    timeout,
		SnapMainland:   MainlandSnap,
		SnapIntl:       InternationalSn,
		OpengwMainland: MainlandOpengw,
		STS:            STSEndpoint,
		IAM:            IAMHost,
	}
}

func (c *Client) snapOf(site string) string {
	if site == store.StationGlobal {
		return strings.TrimRight(c.SnapIntl, "/")
	}
	return strings.TrimRight(c.SnapMainland, "/")
}

func (c *Client) opengwOf(site string) string {
	if site == store.StationGlobal {
		return "" // 国际站没有 opengw
	}
	return strings.TrimRight(c.OpengwMainland, "/")
}

// ---------- 签名请求 ----------

// signedDo 发一次已签名的请求。
//
// unsigned 里的头在签名之后追加（Agent-Type / X-Language），它们不参与
// 签名——签名了反而对不上。extra 里的头参与签名（例如 maas_type）。
func (c *Client) signedDo(ctx context.Context, cred Credential, method, rawURL string, body []byte,
	extra map[string]string, unsigned map[string]string) (*http.Response, []byte, error) {
	headers, err := SignRequest(cred.AccessKeyID, cred.SecretAccessKey, cred.SecurityToken,
		method, rawURL, body, extra, time.Now())
	if err != nil {
		return nil, nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range unsigned {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Transport: c.HTTP.Transport,
		Timeout:   c.RequestTimeout,
		// 绝不带着临时凭据跟随跳转。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, &raccoon.UpstreamError{Status: 0, Message: "CodeArts 上游连接失败: " + err.Error(), Endpoint: rawURL}
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	res.Body.Close()
	return res, raw, nil
}

// openChatURL 是账号站点的对话地址。
func (c *Client) openChatURL(site string) string { return c.snapOf(site) + chatPath }

// OpenChat 发起一次对话，返回统一形状的流。
func (c *Client) OpenChat(ctx context.Context, acc *store.Account, body map[string]any) (*raccoon.ChatStream, error) {
	cred, err := DecodeCred(acc)
	if err != nil {
		return nil, &raccoon.UpstreamError{Status: 401, Auth: true, Message: err.Error()}
	}
	site := acc.Station()
	prepared, opts := PrepareChatBody(body, site)
	model, _ := prepared["model"].(string)
	raw, err := json.Marshal(prepared)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSignedBody {
		return nil, &raccoon.UpstreamError{Status: http.StatusRequestEntityTooLarge, Message: "CodeArts 请求体超过 12MB 上限"}
	}

	// 免费模型要带 maas_type: benefit，否则会回「模型未注册」。
	extra := map[string]string{}
	if opts.Benefit {
		extra["maas_type"] = "benefit"
	}

	sessionID, chatID := AccountSessionIDs(cred)
	unsigned := map[string]string{
		"Chat-Id":                chatID,
		"Session-Id":             sessionID,
		"x-ot-session-id":        sessionID,
		"user-session-id":        sessionID,
		"x-ot-parent-session-id": "",
		"x-ot-trace-id":          randomHex(16),
		"x-ot-span-id":           randomHex(8),
		"x-snap-traceid":         randomHex(16),
		"lang":                   "en",
		"model-id":               model,
		"model-name":             model,
		"x-model-id":             model,
	}

	streamCtx, cancel := context.WithCancel(ctx)
	url := c.openChatURL(site)
	headers, err := SignRequest(cred.AccessKeyID, cred.SecretAccessKey, cred.SecurityToken,
		http.MethodPost, url, raw, extra, time.Now())
	if err != nil {
		cancel()
		return nil, err
	}
	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range unsigned {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")

	res, err := (&http.Client{Transport: c.HTTP.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		cancel()
		return nil, &raccoon.UpstreamError{Status: 0, Message: "CodeArts 上游连接失败: " + err.Error(), Endpoint: url}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		rawBody, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		cancel()
		return nil, classifyHTTPError(res.StatusCode, res.Header, rawBody, url)
	}

	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	stream := raccoon.NewChatStream(res, sc, cancel, ctx, c.IdleTimeout)
	if len(opts.ToolNames) > 0 {
		names := opts.ToolNames
		stream.OnEvent = func(ev *raccoon.ChatEvent) {
			if ev.Delta == nil {
				return
			}
			for i := range ev.Delta.ToolCalls {
				ev.Delta.ToolCalls[i].Function.Name = restoreName(ev.Delta.ToolCalls[i].Function.Name, names)
			}
		}
	}
	return stream, nil
}

// maxSignedBody 是 APIG 的请求体上限（12 MiB）。
const maxSignedBody = 12 << 20

// ---------- 错误分类 ----------

// classifyHTTPError 把非 2xx 折成 UpstreamError。
func classifyHTTPError(status int, header http.Header, body []byte, endpoint string) *raccoon.UpstreamError {
	code := errorCodeOf(body)
	msg := firstNonEmpty(errorMessageOf(body), strings.TrimSpace(string(body)))
	ue := &raccoon.UpstreamError{Status: status, Message: shorten(msg, 500), Endpoint: endpoint}
	switch {
	case IsStaleCredential(status, string(body)):
		// 临时凭据过期：由转发层按 Auth 处理并续期重试。
		ue.Auth = true
	case status == http.StatusUnauthorized:
		ue.Auth = true
	case status == http.StatusForbidden:
		// 403 在码道多半是额度或权限；只有文案明确指向登录态才算 Auth。
		if strings.Contains(strings.ToLower(msg), "quota") || strings.Contains(msg, "额度") {
			ue.Status = http.StatusTooManyRequests
		}
	case status == http.StatusTooManyRequests:
		ue.RetryAfter = raccoon.RetryAfter(header, body)
	case isNotMultimodal(code, msg):
		// 模型不吃图片：这是请求侧问题，回 400 而不是让 agent 反复重试的 502。
		ue.Status = http.StatusBadRequest
	case embeddedErrorStatus(code) == http.StatusTooManyRequests:
		ue.Status = http.StatusTooManyRequests
	}
	if ue.Message == "" {
		ue.Message = fmt.Sprintf("CodeArts 上游 HTTP %d", status)
	}
	return ue
}
