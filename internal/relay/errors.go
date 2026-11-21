package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// errClientGone 表示这次失败与上游无关：客户端断开、写回失败或服务正在关闭。
// 它不能按上游故障去冷却账号，也不能继续换号重试——请求方已经不在了。
var errClientGone = errors.New("客户端已断开，本次请求终止")

// classify 把上游错误翻成号池冷却分类与是否值得换号重试。
func classify(err error) (pool.FailKind, bool, int, string) {
	if isClientGone(err) {
		return pool.FailOther, false, 499, "客户端已断开"
	}
	// 排队超时是我们自己的准入控制，不是上游故障：不换号、不冷却。
	if isQueueTimeout(err) {
		return pool.FailOther, false, http.StatusServiceUnavailable, err.Error()
	}
	var ue *raccoon.UpstreamError
	if !errors.As(err, &ue) {
		return pool.FailTransient, true, http.StatusBadGateway, "上游请求失败"
	}
	msg := cleanMessage(ue.Error())
	switch {
	case isContentRejectedCode(ue.Code):
		// 100041 / 100042 是上游对内容本身的拒答（命中敏感词），跟账号无关：
		// 换号重试只会拿另一个号再问一遍同样的话，必然还是拒答。
		// 不冷却、不换号，直接把错误交给调用方。
		return pool.FailRejected, false, http.StatusBadRequest, msg
	case ue.Code == codeAccountAbnormal:
		// 200108「系统检测到当前账号存在异常调用」：上游罚的是整个账号，
		// 不是某一次调用，换模型也绕不过去。按账号级阶梯冷却处理。
		return pool.FailAccountAbnormal, true, http.StatusTooManyRequests, msg
	case ue.Status == http.StatusUnauthorized || ue.Auth:
		return pool.FailRelogin, true, http.StatusUnauthorized, msg
	case ue.Status == http.StatusForbidden:
		// 403 不等于凭证过期：上游对设备指纹异常、风控、权限不足都会回 403。
		// 只有文案明确指向登录态时才判 relogin，其余视为上游波动：
		// 不冷却账号，直接换号重试，否则一个 403 就让这个号白停两小时。
		if looksLikeAuthFailure(ue.Message) {
			return pool.FailRelogin, true, http.StatusUnauthorized, msg
		}
		return pool.FailTransient, true, http.StatusForbidden, blockedMessage(msg)
	case ue.Status == http.StatusPaymentRequired:
		return pool.FailQuota, true, http.StatusPaymentRequired, msg
	case ue.Status == http.StatusTooManyRequests:
		return pool.FailRate, true, http.StatusTooManyRequests, msg
	case ue.Code == codeRefreshConflict:
		// 并发刷新冲突：不是凭证失效，换号重试即可，别把账号打成 relogin。
		return pool.FailTransient, true, http.StatusBadGateway, msg
	case ue.Status >= 500:
		return pool.FailTransient, true, http.StatusBadGateway, msg
	case ue.Status == 0:
		return pool.FailTransient, true, http.StatusBadGateway, msg
	}
	return pool.FailOther, false, http.StatusBadRequest, msg
}

// isTokenExpiredErr 判断错误是否在说「这枚 access 过期了 / 不认这枚 access」。
// 401 和显式 Auth 标记都算；403 要在本地 exp 已过期、或文案明确指向登录态时才算
// （上游对过期 access 有时回 403，但风控/权限不足同样是 403）。
func isTokenExpiredErr(err error, tokenExp int64) bool {
	var ue *raccoon.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	if ue.Auth || ue.Status == http.StatusUnauthorized {
		return true
	}
	if ue.Status != http.StatusForbidden {
		return false
	}
	if tokenExp > 0 && tokenExp <= time.Now().UnixMilli() {
		return true
	}
	return looksLikeAuthFailure(ue.Message)
}

// shouldRefreshRetry 判断一次对话请求失败是否值得「强刷令牌后重放一次」。
//
// 只认「access 过期/不可用」这一类。普通 403（风控、权限不足、设备指纹异常）
// **不**强刷——白刷只会轮换 refresh_token，把桌面端的登录态一起顶掉，
// 而问题根本不在令牌上。
func shouldRefreshRetry(err error, tokenExp int64) bool {
	return isTokenExpiredErr(err, tokenExp)
}

// 上游业务错误码。这些码不依赖 HTTP 状态，必须按 code 判断。
const (
	// codeAccountAbnormal = 200108：上游检测到账号存在异常调用，模型调用功能暂时受限。
	// 这是账号级处罚，用状态码分类会漏掉（它有时挂在 4xx、有时挂在 200 的错误帧上）。
	codeAccountAbnormal = 200108
	// codeRefreshConflict = 200822：refresh_token 已被并发刷新替换。
	codeRefreshConflict = 200822
	// codeContentRejected = 100041：上游因内容命中敏感词拒答。这是请求本身的问题，
	// 换号、重试都不会改变结果，也不该让任何账号吃冷却。
	codeContentRejected = 100041
	// codeSensitiveContent = 100042：上游另一条「请求内容包含敏感内容」的拒答码，
	// 与 100041 同义（下游请求里的内容被判敏感），走同一套处理：不重试、回 400。
	codeSensitiveContent = 100042
)

// isContentRejectedCode 判断业务码是否属于「请求内容被上游拒答」。
// 这类错误只取决于下游发来的内容，与账号无关，因此永不冷却、永不换号。
func isContentRejectedCode(code int) bool {
	return code == codeContentRejected || code == codeSensitiveContent
}

// isModelScoped 判断错误是否只针对「账号 × 模型」：换号无用，罚模型即可。
func isModelScoped(err error) bool {
	var ue *raccoon.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	// 6004 = 模型级限流，11102 = 该账号不支持此模型
	return ue.Code == 6004 || ue.Code == 11102
}

// isClientGone 判断错误是否来自「请求方已经不在」而非上游。
//
// 客户端的 ctx 被取消（App 取消请求、页面关掉）时，http.Client 会返回
// context.Canceled；往已断开的连接写 SSE 会返回 write: broken pipe。
// 这两类失败必须从「上游故障」里摘出去，否则一次用户取消就白罚一个号 10 分钟，
// 还会继续拿别的号重试——既浪费额度，又会在号池里制造假故障。
func isClientGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, errClientGone) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, w := range []string{"broken pipe", "client disconnected", "connection reset by peer", "use of closed network connection"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func statusFor(kind pool.FailKind) int {
	switch kind {
	case pool.FailQuota:
		return http.StatusPaymentRequired
	case pool.FailRate, pool.FailAccountAbnormal:
		return http.StatusTooManyRequests
	case pool.FailRelogin:
		return http.StatusUnauthorized
	case pool.FailRejected:
		return http.StatusBadRequest
	case pool.FailTransient:
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

// isRefreshConflict 判断错误是否是「refresh_token 已被并发刷新替换」。
// 这不是凭证失效：并发的另一路刷新刚把凭据轮换掉，重新读库用最新值再来一次就好。
func isRefreshConflict(err error) bool {
	var ue *raccoon.UpstreamError
	return errors.As(err, &ue) && ue.Code == codeRefreshConflict
}

// authHintWords 是上游把 403 文案写成登录态问题时才会出现的词。
// 注意不要包含「过期」：403 的常见文案是「请求已过期/签名过期」，跟登录态无关。
var authHintWords = []string{
	"authorization_verify_error",
	"登录态",
	"token expired",
	"token 过期",
	"token 已过期",
	"unauthorized",
	"未登录",
	"请登录",
	"重新登录",
}

// looksLikeAuthFailure 判断 403 的文案是否真的在说「凭证不能用」。
func looksLikeAuthFailure(msg string) bool {
	low := strings.ToLower(strings.TrimSpace(msg))
	if low == "" {
		return false
	}
	for _, w := range authHintWords {
		if strings.Contains(low, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

// blockedMessage 给 403 一个对外可读、不误导为「账号过期」的文案。
func blockedMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" || strings.HasPrefix(msg, "HTTP 403") {
		return "上游返回 403（风控或权限拦截，非凭证过期），已换号重试"
	}
	return cleanMessage(msg)
}

func cleanMessage(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "上游请求失败"
	}
	if len([]rune(s)) > 300 {
		return string([]rune(s)[:300])
	}
	return s
}

// diagnosticBlock 把一次上游失败的现场整理成多行文本，写进运行事件。
// 面板上的「为什么 403」靠的就是这段：状态码、上游响应头、原始响应体，
// 三者缺一不可——网关拦截常常只体现在响应头上，响应体是空的。
func diagnosticBlock(ue *raccoon.UpstreamError) string {
	if ue == nil {
		return ""
	}
	var b strings.Builder
	if ue.Endpoint != "" {
		fmt.Fprintf(&b, "接口：%s\n", ue.Endpoint)
	}
	if ue.TurnID != "" {
		fmt.Fprintf(&b, "turn：%s\n", ue.TurnID)
	}
	if ue.RequestID != "" {
		fmt.Fprintf(&b, "上游请求ID：%s\n", ue.RequestID)
	}
	if ue.RequestSummary != "" {
		b.WriteString("请求摘要：\n")
		for _, line := range strings.Split(ue.RequestSummary, "\n") {
			b.WriteString("  " + line + "\n")
		}
	}
	if len(ue.Headers) > 0 {
		keys := make([]string, 0, len(ue.Headers))
		for k := range ue.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("响应头：\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s: %s\n", k, ue.Headers[k])
		}
	}
	if ue.Raw != "" {
		b.WriteString("响应体：\n")
		b.WriteString(ue.Raw)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
