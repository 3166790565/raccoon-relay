package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// Quota 是 AutoClaw 的一次额度快照。
//
// 它是纯积分制：没有「总量 / 已用」，只有一个余额。细分钱包（赠送、会员…）
// 只做展示，不参与路由——路由只看 Total。
type Quota struct {
	Total   float64  `json:"total"`
	Wallets []Wallet `json:"wallets,omitempty"`
	At      int64    `json:"at"`
}

// Wallet 是钱包明细的一行。
type Wallet struct {
	Name    string  `json:"name"`
	Balance float64 `json:"balance"`
}

// Wallets 接口的原始形状：{code, msg, data:{total_balance, wallets:[...]}}。
type rawWallets struct {
	TotalBalance json.RawMessage `json:"total_balance"`
	Wallets      []struct {
		DisplayName string          `json:"display_name"`
		PublicType  string          `json:"public_wallet_type"`
		Balance     json.RawMessage `json:"balance"`
		Display     *bool           `json:"display"`
	} `json:"wallets"`
}

// FetchQuota 读余额。返回的 Quota.Total < 0 表示上游响应异常（字段缺失或非数），
// 调用方应保留旧值，而不是把余额写成 0。
func (c *Client) FetchQuota(ctx context.Context, acc *store.Account) (Quota, error) {
	cred, err := DecodeCred(acc)
	if err != nil {
		return Quota{}, err
	}
	if cred.Access == "" {
		return Quota{}, &QuotaError{Msg: "AutoClaw 凭据缺少 access_token"}
	}
	env, err := c.doBusiness(ctx, c.hostFor(acc), http.MethodGet, pathWallets, cred.Access, nil)
	if err != nil {
		return Quota{}, err
	}
	var raw rawWallets
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &raw); err != nil {
			return Quota{Total: -1, At: time.Now().UnixMilli()}, &QuotaError{Msg: "AutoClaw 余额响应无法解析"}
		}
	}
	total, ok := parseMoney(raw.TotalBalance)
	if !ok {
		return Quota{Total: -1, At: time.Now().UnixMilli()}, &QuotaError{Msg: "AutoClaw 余额响应异常（缺少 total_balance）"}
	}
	q := Quota{Total: total, At: time.Now().UnixMilli()}
	for _, w := range raw.Wallets {
		if w.Display != nil && !*w.Display {
			continue
		}
		bal, ok := parseMoney(w.Balance)
		if !ok || bal <= 0 {
			continue
		}
		// 与总额相同的钱包不再重复列出，否则面板上会出现「余额 734 / 赠送 734」。
		if roundMoney(bal) == roundMoney(total) {
			continue
		}
		q.Wallets = append(q.Wallets, Wallet{Name: firstNonEmpty(w.DisplayName, w.PublicType, "积分"), Balance: bal})
		if len(q.Wallets) >= 3 {
			break
		}
	}
	return q, nil
}

// QuotaError 是余额接口的「响应异常」：账号本身没问题，只是这次读不到。
// 与 UpstreamError 区分开，避免把余额响应异常当成上游故障去冷却账号。
type QuotaError struct{ Msg string }

func (e *QuotaError) Error() string { return e.Msg }

// MarshalStored 序列化成面板展示用的 JSON。
func (q Quota) MarshalStored() string {
	raw, _ := json.Marshal(q)
	return string(raw)
}

// UnmarshalQuota 从账号上还原上次的额度快照。解析失败返回 nil。
func UnmarshalQuota(raw string) *Quota {
	if raw == "" {
		return nil
	}
	var q Quota
	if json.Unmarshal([]byte(raw), &q) != nil {
		return nil
	}
	return &q
}

// Summary 是面板与日志用的一行摘要。
func (q Quota) Summary() string {
	if q.Total < 0 {
		return "余额未知"
	}
	s := fmt.Sprintf("%.0f 积分", q.Total)
	for _, w := range q.Wallets {
		s += " · " + w.Name + fmt.Sprintf(" %.0f", w.Balance)
	}
	return s
}

// parseMoney 容忍数字、数字字符串与 null。null / 空串按「未知」处理。
func parseMoney(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var v float64
		if _, err := fmt.Sscanf(s, "%f", &v); err == nil {
			return v, true
		}
	}
	return 0, false
}

func roundMoney(v float64) float64 {
	if v < 0 {
		v = -v
	}
	return float64(int64(v + 0.5))
}
