package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// Quota 是码道的一次额度快照。
//
// 码道的额度是「套餐积分桶」：总积分（total）决定号能不能用，基础/按需/
// 赠送桶只做展示。免费额度（daily）是另一条线，按天重置。
type Quota struct {
	Plan    string   `json:"plan"`
	Total   float64  `json:"total"`
	Remain  float64  `json:"remain"`
	Buckets []Bucket `json:"buckets,omitempty"`
	// Daily 是免费额度（tokens），resetAt 为下一个北京时间零点。
	DailyLimit float64 `json:"dailyLimit,omitempty"`
	DailyUsed  float64 `json:"dailyUsed,omitempty"`
	At         int64   `json:"at"`
}

// Bucket 是一个积分桶。
type Bucket struct {
	Name   string  `json:"name"`
	Limit  float64 `json:"limit"`
	Remain float64 `json:"remain"`
}

// FetchQuota 读额度。失败返回 error；响应异常时 err 为 *QuotaError，
// 调用方应保留旧值而不是写 0。
func (c *Client) FetchQuota(ctx context.Context, acc *store.Account) (Quota, error) {
	cred, err := DecodeCred(acc)
	if err != nil {
		return Quota{}, err
	}
	site := acc.Station()
	rawURL := c.snapOf(site) + "/snap-manager/v1/statistics/plugin"
	_, body, err := c.signedDo(ctx, cred, http.MethodGet, rawURL, nil,
		map[string]string{"content-type": "application/json"},
		map[string]string{"Agent-Type": "PromptCenter", "X-Language": "zh-cn"})
	if err != nil {
		return Quota{}, err
	}
	q, err := parseStatistics(body, site)
	if err != nil {
		return q, err
	}
	// 免费额度（仅国内站有 opengw）。
	if gw := c.opengwOf(site); gw != "" {
		if lim, used, ok := c.fetchDailyPool(ctx, cred, gw); ok {
			q.DailyLimit, q.DailyUsed = lim, used
		}
	}
	q.At = time.Now().UnixMilli()
	return q, nil
}

func parseStatistics(body []byte, site string) (Quota, error) {
	var env struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Data    struct {
			Package struct {
				NameCN    string `json:"package_name_cn"`
				NameEN    string `json:"package_name_en"`
				SpecCode  string `json:"spec_code"`
				TokenPlan bool   `json:"is_token_package"`
			} `json:"package"`
			Metrics []struct {
				Name       string          `json:"name"`
				Limit      json.RawMessage `json:"package_credit_amount"`
				Remain     json.RawMessage `json:"package_credit_remain"`
				TokenLimit json.RawMessage `json:"package_token_amount"`
				TokenUsed  json.RawMessage `json:"usage_token_num"`
			} `json:"metrics"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Quota{Total: -1}, &QuotaError{Msg: "CodeArts 额度响应无法解析"}
	}
	if code := strOf(jsonValue(env.Code)); code != "" && code != "0" && code != "0000" {
		return Quota{Total: -1}, &QuotaError{Msg: firstNonEmpty(env.Message, "CodeArts 额度查询被拒绝（code "+code+"）")}
	}
	plan := env.Data.Package.NameCN
	if site == store.StationGlobal {
		plan = firstNonEmpty(env.Data.Package.NameEN, plan)
	}
	plan = firstNonEmpty(plan, env.Data.Package.SpecCode)
	q := Quota{Plan: plan, Total: -1}

	for _, m := range env.Data.Metrics {
		limit := numOf(jsonValue(m.Limit))
		remain := numOf(jsonValue(m.Remain))
		if limit > 0 || remain > 0 {
			b := Bucket{Name: metricLabel(m.Name), Limit: limit, Remain: remain}
			q.Buckets = append(q.Buckets, b)
			if m.Name == "usageTotalPackageCredit" {
				q.Total, q.Remain = limit, remain
			}
		}
	}
	// token 套餐：总 token 额度窗口。
	if q.Total < 0 {
		for _, m := range env.Data.Metrics {
			lim := numOf(jsonValue(m.TokenLimit))
			if lim > 0 {
				used := numOf(jsonValue(m.TokenUsed))
				q.Total, q.Remain = lim, lim-used
				q.Buckets = append(q.Buckets, Bucket{Name: "Token 额度", Limit: lim, Remain: lim - used})
				break
			}
		}
	}
	if q.Total < 0 && len(q.Buckets) == 0 {
		return q, &QuotaError{Msg: "该账号没有积分额度"}
	}
	if q.Total < 0 {
		// 没有总积分桶时，用各桶之和近似，保证面板有数可看。
		for _, b := range q.Buckets {
			q.Total += b.Limit
			q.Remain += b.Remain
		}
	}
	return q, nil
}

func metricLabel(name string) string {
	switch name {
	case "usageTotalPackageCredit":
		return "总积分"
	case "usageBasicPackageCredit":
		return "基础积分包"
	case "usageOnDemandPackageCredit":
		return "按需积分包"
	case "usageBonusPackageCredit":
		return "赠送积分包"
	case "usageTokenChatMessages":
		return "对话次数"
	}
	return firstNonEmpty(name, "额度")
}

// fetchDailyPool 读免费日额度（tokens）。失败返回 ok=false。
func (c *Client) fetchDailyPool(ctx context.Context, cred Credential, opengw string) (float64, float64, bool) {
	rawURL := opengw + "/api/v1/user/tokens/balance"
	_, body, err := c.signedDo(ctx, cred, http.MethodGet, rawURL, nil, nil, nil)
	if err != nil {
		return 0, 0, false
	}
	var v struct {
		ErrorCode string         `json:"error_code"`
		Result    map[string]any `json:"result"`
		Data      map[string]any `json:"data"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0, 0, false
	}
	if v.ErrorCode != "" && v.ErrorCode != "0000" {
		return 0, 0, false
	}
	src := v.Result
	if src == nil {
		src = v.Data
	}
	if src == nil {
		return 0, 0, false
	}
	lim := numOf(src["daily_token_limit"])
	used := numOf(src["daily_tokens_used"])
	if lim <= 0 {
		return 0, 0, false
	}
	return lim, used, true
}

// QuotaError 是「读到了但响应异常」的情况，区别于上游故障。
type QuotaError struct{ Msg string }

func (e *QuotaError) Error() string { return e.Msg }

// MarshalStored 序列化成面板展示用 JSON。
func (q Quota) MarshalStored() string {
	raw, _ := json.Marshal(q)
	return string(raw)
}

// UnmarshalQuota 从账号上还原上次的额度快照。
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

func (q Quota) Summary() string {
	if q.Total < 0 {
		return "额度未知"
	}
	s := fmt.Sprintf("%.0f/%.0f 积分", q.Remain, q.Total)
	if q.DailyLimit > 0 {
		s += fmt.Sprintf(" · 免费 %.0f/%.0f tokens", q.DailyUsed, q.DailyLimit)
	}
	return s
}
