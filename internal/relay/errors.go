package relay

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// classify 把上游错误翻成号池冷却分类与是否值得换号重试。
func classify(err error) (pool.FailKind, bool, int, string) {
	var ue *raccoon.UpstreamError
	if !errors.As(err, &ue) {
		return pool.FailServer, true, http.StatusBadGateway, "上游请求失败"
	}
	msg := cleanMessage(ue.Error())
	switch {
	case ue.Status == http.StatusUnauthorized || ue.Auth:
		return pool.FailRelogin, true, http.StatusUnauthorized, msg
	case ue.Status == http.StatusForbidden:
		// 403 不等于凭证过期：上游对设备指纹异常、风控、权限不足都会回 403。
		// 只有文案明确指向登录态时才判 relogin，其余按风控冷却并换号，
		// 否则一个 403 就会把这个号永久打成「需重登」。
		if looksLikeAuthFailure(ue.Message) {
			return pool.FailRelogin, true, http.StatusUnauthorized, msg
		}
		return pool.FailBlocked, true, http.StatusForbidden, blockedMessage(msg)
	case ue.Status == http.StatusPaymentRequired:
		return pool.FailQuota, true, http.StatusPaymentRequired, msg
	case ue.Status == http.StatusTooManyRequests:
		return pool.FailRate, true, http.StatusTooManyRequests, msg
	case ue.Status >= 500:
		return pool.FailServer, true, http.StatusBadGateway, msg
	case ue.Status == 0:
		return pool.FailServer, true, http.StatusBadGateway, msg
	case ue.Code == 200822:
		// 并发刷新冲突：不是凭证失效，换号重试即可，别把账号打成 relogin。
		return pool.FailServer, true, http.StatusBadGateway, msg
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

// isModelScoped 判断错误是否只针对「账号 × 模型」：换号无用，罚模型即可。
func isModelScoped(err error) bool {
	var ue *raccoon.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	// 6004 = 模型级限流，11102 = 该账号不支持此模型
	return ue.Code == 6004 || ue.Code == 11102
}

func statusFor(kind pool.FailKind) int {
	switch kind {
	case pool.FailQuota:
		return http.StatusPaymentRequired
	case pool.FailRate:
		return http.StatusTooManyRequests
	case pool.FailRelogin:
		return http.StatusUnauthorized
	case pool.FailBlocked:
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

// isRefreshConflict 判断错误是否是「refresh_token 已被并发刷新替换」。
// 这不是凭证失效：并发的另一路刷新刚把凭据轮换掉，重新读库用最新值再来一次就好。
func isRefreshConflict(err error) bool {
	var ue *raccoon.UpstreamError
	return errors.As(err, &ue) && ue.Code == 200822
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
