package raccoon

import "strings"

// 上游业务码与文案的判定规则。放在一个文件里，是为了让「哪些码算凭证失效 /
// 哪些算内容拦截」只有一份定义——转发层与各控制面端点各自抄一份迟早会漂。

const (
	// CodeTokenExpired 是「这枚 access 不可用」的数字码。
	CodeTokenExpired = 200003
	// CodeTokenExpiredAlt 是另一条同类数字码（上游不同网关各用一条）。
	CodeTokenExpiredAlt = 200009
	// CodeIamConfirmRequired 是 IAM 首次迁移要确认一次的业务码。
	CodeIamConfirmRequired = 200901
	// CodeAuthVerifyErrorName 是凭证失效的文本码。
	CodeAuthVerifyErrorName = "authorization_verify_error"
)

// IsAuthCode 判断业务码是否在说「凭证不可用」。
//
// 数字码（200003 / 200009）与文本码（authorization_verify_error）都认：
// 上游对不同端点的同一件事会换形态下发，只认其中一种就会漏掉另一条链路，
// 表现为「余额/目录一直查不到数据，但号看着是好的」。
func IsAuthCode(code int, codeName string) bool {
	switch code {
	case CodeTokenExpired, CodeTokenExpiredAlt:
		return true
	}
	return strings.EqualFold(strings.TrimSpace(codeName), CodeAuthVerifyErrorName)
}

// LooksLikeAuthMessage 判断文案是否在说「凭证不可用」。
func LooksLikeAuthMessage(msg string) bool {
	return strings.Contains(strings.ToLower(msg), CodeAuthVerifyErrorName)
}

// contentPolicyPatterns 是上游内容策略拦截的**逐字**文案特征（全部小写）。
//
// 上游按逐字精确匹配审核，客户端注入的 system 模板句命中即整单拦截；
// 此时账号本身是健康的（余额够、没限流、会话没死），换号再试是白扔另一个
// 号的额度——同一份 body 换谁发都会被拦。所以这一类必须与「账号问题」分开。
var contentPolicyPatterns = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// IsContentPolicy 判断错误文案是否命中上游内容策略拦截。
func IsContentPolicy(message string) bool {
	low := strings.ToLower(message)
	for _, pattern := range contentPolicyPatterns {
		if strings.Contains(low, pattern) {
			return true
		}
	}
	return false
}
