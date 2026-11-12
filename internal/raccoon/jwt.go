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

// AccountUID 按 sub → uid → id → iss 的顺序取，与桌面版口径一致。
func (c *Claims) AccountUID() string {
	for _, v := range []string{c.Sub, c.UID, c.ID, c.ISS} {
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
