package relay

import (
	"errors"
	"net/http"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// classify 把上游错误翻成号池冷却分类与是否值得换号重试。
func classify(err error) (pool.FailKind, bool, int, string) {
	var ue *raccoon.UpstreamError
	if !errors.As(err, &ue) {
		return pool.FailServer, true, http.StatusBadGateway, "上游请求失败"
	}
	msg := ue.Error()
	switch {
	case ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden || ue.Auth:
		return pool.FailRelogin, true, http.StatusUnauthorized, msg
	case ue.Status == http.StatusPaymentRequired:
		return pool.FailQuota, true, http.StatusPaymentRequired, msg
	case ue.Status == http.StatusTooManyRequests:
		return pool.FailRate, true, http.StatusTooManyRequests, msg
	case ue.Status >= 500:
		return pool.FailServer, true, http.StatusBadGateway, msg
	case ue.Status == 0:
		return pool.FailServer, true, http.StatusBadGateway, msg
	case ue.Code == 200822:
		return pool.FailRelogin, false, http.StatusUnauthorized, msg
	}
	return pool.FailOther, false, http.StatusBadRequest, msg
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
	default:
		return http.StatusBadGateway
	}
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
