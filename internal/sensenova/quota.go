package sensenova

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PoolWindow 是一个积分池里的单个窗口（5 小时 / 本周）。
type PoolWindow struct {
	Limit     float64 `json:"limit"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	ResetAt   int64   `json:"resetAt"` // 秒级 Unix；0 表示接口没给
}

// Pool 是控制台返回的一个积分池。
type Pool struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Type      string      `json:"type"` // default / dedicated
	ModelIDs  []string    `json:"modelIds"`
	Window5h  *PoolWindow `json:"window5h"`
	Window7d  *PoolWindow `json:"window7d"`
	Grant     float64     `json:"grant"` // 活动固定积分余额
	GrantHave bool        `json:"grantHave"`
}

// Usage 是一次额度查询的归一化结果。
type Usage struct {
	Plan  string `json:"plan"`
	Pools []Pool `json:"pools"`
	// At 是这次查询的时间（毫秒），供面板显示「何时查的」。
	At int64 `json:"at"`
}

// PoolUsage 是控制台积分池接口的原始响应。
type PoolUsage struct {
	Plan  map[string]any `json:"plan"`
	Pools []rawPool      `json:"pools"`
}

type rawPool struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	PoolType     string        `json:"pool_type"`
	ModelIDs     []string      `json:"model_ids"`
	Window5h     *rawWindow    `json:"window_5h"`
	Window7d     *rawWindow    `json:"window_7d"`
	GrantBalance flexibleFloat `json:"grant_balance"`
}

type rawWindow struct {
	Limit     flexibleFloat `json:"limit"`
	Used      flexibleFloat `json:"used"`
	Remaining flexibleFloat `json:"remaining"`
	ResetAt   flexibleFloat `json:"reset_at"`
}

// flexibleFloat 兼容数字与数字字符串（上游两种都出现过）。
type flexibleFloat float64

func (f *flexibleFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexibleFloat(v)
	return nil
}

func (f flexibleFloat) float64() float64 { return float64(f) }

// FetchUsage 读控制台积分池。consoleToken 是平台登录票，accountID 是登录票里的 tenant_id。
func (c *Client) FetchUsage(ctx context.Context, consoleToken, accountID string) (*Usage, error) {
	if strings.TrimSpace(consoleToken) == "" {
		return nil, newError("quota", "没有登录票，无法读取额度", "")
	}
	target := c.Lite + "/console/v1/tokenplan/pool-usage"
	if accountID != "" {
		target += "?account_id=" + urlQueryEscape(accountID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+consoleToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Referer", c.Platform+"/console")
	req.Header.Set("User-Agent", UserAgent)
	res, err := c.httpClient().Do(req)
	if err != nil {
		return nil, newError("quota", "读取额度请求失败", err.Error())
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 400 {
		var d struct {
			Details []struct {
				Message string `json:"message"`
			} `json:"details"`
		}
		_ = json.Unmarshal(raw, &d)
		detail := fmt.Sprintf("HTTP %d", res.StatusCode)
		for _, x := range d.Details {
			if x.Message != "" {
				detail = x.Message
				break
			}
		}
		return nil, newError("quota", "读取额度失败", shorten(detail, 200))
	}
	var pu PoolUsage
	if err := json.Unmarshal(raw, &pu); err != nil {
		return nil, newError("quota", "额度响应不是合法 JSON", shorten(string(raw), 160))
	}
	return normalizeUsage(&pu), nil
}

// normalizeUsage 把控制台响应映射成面板可用的结构。
func normalizeUsage(pu *PoolUsage) *Usage {
	out := &Usage{At: time.Now().UnixMilli()}
	if name, ok := pu.Plan["name"].(string); ok {
		out.Plan = strings.TrimSpace(name)
	}
	for _, rp := range pu.Pools {
		pool := Pool{
			ID:       rp.ID,
			Name:     strings.TrimSpace(rp.Name),
			Type:     rp.PoolType,
			ModelIDs: rp.ModelIDs,
		}
		if pool.Name == "" {
			pool.Name = "积分池"
		}
		pool.Window5h = normalizeWindow(rp.Window5h)
		pool.Window7d = normalizeWindow(rp.Window7d)
		if rp.GrantBalance != 0 {
			pool.Grant, pool.GrantHave = rp.GrantBalance.float64(), true
		}
		out.Pools = append(out.Pools, pool)
	}
	return out
}

func normalizeWindow(w *rawWindow) *PoolWindow {
	if w == nil {
		return nil
	}
	limit := w.Limit.float64()
	if limit <= 0 {
		return nil
	}
	used := w.Used.float64()
	remaining := w.Remaining.float64()
	// 上游只给 limit + remaining 时反推已用；只给 used 时反推剩余。
	if remaining <= 0 && used > 0 {
		remaining = limit - used
	}
	if used <= 0 && remaining > 0 && remaining < limit {
		used = limit - remaining
	}
	if remaining < 0 {
		remaining = 0
	}
	if used < 0 {
		used = 0
	}
	return &PoolWindow{Limit: limit, Used: used, Remaining: remaining, ResetAt: int64(w.ResetAt.float64())}
}

// 较短窗口的 label，面板与刻度用。
const (
	Window5hLabel = "5 小时"
	Window7dLabel = "本周"
)

// Windows 把池展开成「池名 · 窗口名」的扁平读数，供面板渲染。
type FlatWindow struct {
	Name      string  `json:"name"`
	Scope     string  `json:"scope"` // 池名
	Label     string  `json:"label"` // 窗口名
	Limit     float64 `json:"limit"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	ResetAt   int64   `json:"resetAt"`
	Percent   float64 `json:"percent"` // 剩余百分比
	Models    string  `json:"models"`  // 逗号分隔的模型清单，可能为空
}

// Flatten 把 Usage 展开成扁平窗口。顺序稳定：先 5 小时再本周，池内按接口顺序。
func (u *Usage) Flatten() []FlatWindow {
	if u == nil {
		return nil
	}
	var out []FlatWindow
	for _, p := range u.Pools {
		for _, pair := range []struct {
			label string
			w     *PoolWindow
		}{
			{Window5hLabel, p.Window5h},
			{Window7dLabel, p.Window7d},
		} {
			w := pair.w
			if w == nil || w.Limit <= 0 {
				continue
			}
			pct := 0.0
			if w.Limit > 0 {
				pct = w.Remaining / w.Limit * 100
			}
			out = append(out, FlatWindow{
				Name: p.Name + " · " + pair.label, Scope: p.Name, Label: pair.label,
				Limit: w.Limit, Used: w.Used, Remaining: w.Remaining, ResetAt: w.ResetAt,
				Percent: round1(pct), Models: strings.Join(p.ModelIDs, ","),
			})
		}
	}
	return out
}

// Total 是「主池剩余」的简化读数：取通用池 5 小时窗口的剩余，没有则取本周，
// 再没有就取第一个窗口。面板上的「积分」一列用它，与小浣熊的 credits 同义。
func (u *Usage) Total() (remaining float64, ok bool) {
	if u == nil {
		return 0, false
	}
	var general *Pool
	for i := range u.Pools {
		if u.Pools[i].Type == "default" {
			general = &u.Pools[i]
			break
		}
	}
	if general == nil && len(u.Pools) > 0 {
		general = &u.Pools[0]
	}
	if general == nil {
		return 0, false
	}
	if general.Window5h != nil {
		return general.Window5h.Remaining, true
	}
	if general.Window7d != nil {
		return general.Window7d.Remaining, true
	}
	return 0, false
}

// EarliestReset 返回最近一次窗口重置的时刻（毫秒，0 表示没有）。
func (u *Usage) EarliestReset() int64 {
	var best int64
	for _, w := range u.Flatten() {
		if w.ResetAt <= 0 {
			continue
		}
		ms := w.ResetAt * 1000
		if best == 0 || ms < best {
			best = ms
		}
	}
	return best
}

// NextReset 返回「额度恢复」的时刻（毫秒，0 表示上游没给）。
//
// 日日新没有每日签到，额度按窗口滚动：5 小时池用尽等 5 小时窗口重置，
// 本周池用尽等 7 日窗口重置。所以 402 之后不能用签到计划去估算恢复时刻，
// 只能看上游给的 reset_at。
//
// 有多格同时用尽时必须取**最晚**的重置时刻：一次调用要同时穿过 5 小时与
// 本周两个窗口，5 小时窗口先补满、7 日窗口还是 0 的话请求照样 402。
// 取最早会提前把号放出来，白撞一次。
//
// 取值顺序：
//  1. 已用尽窗口里最晚的重置时刻（真正决定「什么时候全家桶都能用」）；
//  2. 没有窗口标记用尽（剩余读数有误差）时，取全部窗口里最早的重置时刻兜底；
//  3. 都没有就返回 0，由调用方回退到默认恢复时刻。
func (u *Usage) NextReset() int64 {
	if u == nil {
		return 0
	}
	var exhausted, any int64
	for _, w := range u.Flatten() {
		if w.ResetAt <= 0 {
			continue
		}
		ms := w.ResetAt * 1000
		if any == 0 || ms < any {
			any = ms
		}
		if w.Remaining <= 0 && ms > exhausted {
			exhausted = ms
		}
	}
	if exhausted > 0 {
		return exhausted
	}
	return any
}

// PoolsExhausted 判断所有声明了额度上限的窗口是否都已用尽。
// 没有可读窗口（Limit 全为 0）时返回 false——「读不到」不等于「没钱了」。
func (u *Usage) PoolsExhausted() bool {
	if u == nil {
		return false
	}
	seen := false
	for _, w := range u.Flatten() {
		seen = true
		if w.Remaining > 0 {
			return false
		}
	}
	return seen
}

// ModelIDs 汇总所有池声明覆盖的模型，去重并排序。
func (u *Usage) ModelIDs() []string {
	if u == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range u.Pools {
		for _, id := range p.ModelIDs {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

func urlQueryEscape(s string) string {
	// tenant_id 是数字/短串，只用最基础的转义即可。
	r := strings.NewReplacer(" ", "%20", "&", "%26", "=", "%3D", "?", "%3F", "#", "%23")
	return r.Replace(s)
}

func (u *Usage) MarshalStored() string {
	raw, _ := json.Marshal(u)
	return string(raw)
}

func UnmarshalStored(s string) *Usage {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var u Usage
	if json.Unmarshal([]byte(s), &u) != nil {
		return nil
	}
	return &u
}
