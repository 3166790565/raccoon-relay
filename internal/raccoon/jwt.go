package raccoon

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Claims 只取反代需要的几个字段。
// 语义（真实 token 实测）：iss 是签发方常量 xiaohuanxiong.com/iam，不是账号 ID；
// sid 是登录会话 ID，每次登录都变；sub 才是稳定的账号 ID。
type Claims struct {
	Sub string `json:"sub"`
	UID string `json:"uid"`
	ID  string `json:"id"`
	ISS string `json:"iss"`
	SID string `json:"sid"`
	Exp int64  `json:"exp"`
}

func DecodeJWT(token string) (*Claims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil, errInvalidJWT
	}
	raw, err := base64.RawURLEncoding.DecodeString(padURL(parts[1]))
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(padURL(parts[1]))
		if err != nil {
			return nil, errInvalidJWT
		}
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errInvalidJWT
	}
	return &c, nil
}

// Issuer 是小浣熊 IAM 的签发方常量。它不参与 uid 计算，但可以用来判别
// 一份多供应商导出里哪些账号的 token 真属于小浣熊。
const Issuer = "xiaohuanxiong.com/iam"

// IsIam 表示这枚 token 由 IAM 签发（没有旧版 refresh grant）。
func (c *Claims) IsIam() bool {
	return strings.HasSuffix(strings.TrimSpace(c.ISS), "/iam")
}

// AccountUID 按 sub → uid → id 的顺序取账号 ID。
//
// **绝不能回落到 iss**：iss 是签发方，IAM 下是常量 `xiaohuanxiong.com/iam`。
// 一旦拿它当 ID，所有 iss 相同（sub 缺失）的号会共用同一个 UID，导入时
// 相互覆盖。解不出就返回空串，让调用方报「需要重新登录」，而不是造一个假号。
func (c *Claims) AccountUID() string {
	for _, v := range []string{c.Sub, c.UID, c.ID} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func padURL(s string) string {
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return s
}

// TokenUID 是包级便捷调用，解不出返回空串。
func TokenUID(token string) string {
	c, err := DecodeJWT(token)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(c.Sub)
}

// DecodeUID 按 sub → uid → id → iss 的兜底链取账号 ID。
func DecodeUID(token string) string {
	c, err := DecodeJWT(token)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(c.AccountUID())
}

// TokenExpiryMillis 把 JWT 的 exp 转成毫秒时间戳，解不出返回 0。
func TokenExpiryMillis(token string) int64 {
	c, err := DecodeJWT(token)
	if err != nil || c.Exp <= 0 {
		return 0
	}
	return c.Exp * 1000
}

var errInvalidJWT = &jwtError{}

type jwtError struct{}

func (*jwtError) Error() string { return "token 不是合法 JWT" }
