package store

import (
	"context"
	"sort"
	"strings"
	"time"
)

// 报表页的数据来源。
//
// 概览里的总量、趋势、热力图都读 request_daily / request_daily_model 这两张
// 日聚合表：报表要显示一年热力图与长区间趋势，直接扫 usage_logs 大表代价太高。
// 最近延迟、成功率明细这类需要逐条数据的数字，才回落到 usage_logs。

// DailyStat 是某一天的汇总。
type DailyStat struct {
	Day          string `json:"day"`
	Requests     int64  `json:"requests"`
	Successful   int64  `json:"successful"`
	Tokens       int64  `json:"tokens"`
	PromptTokens int64  `json:"promptTokens"`
	Completion   int64  `json:"completionTokens"`
}

// DaySlice 是「某天 × 某个维度值」的用量，模型/协议/账号共用。
type DaySlice struct {
	Day      string `json:"day"`
	Key      string `json:"key"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// ReportRange 是报表的时间窗口（闭区间，按自然日）。
type ReportRange struct {
	Start time.Time
	End   time.Time
	Label string
}

// RangeDays 返回区间覆盖的天数（含首尾）。
func (r ReportRange) RangeDays() int {
	if r.End.Before(r.Start) {
		return 0
	}
	return int(r.End.Sub(r.Start)/(24*time.Hour)) + 1
}

// NormalizeReportRange 把面板传来的 range 收敛为具体日期。
//
// 口径与常见报表页一致：today/yesterday 各取一整天，7/30 从今天往前数 N-1 天，
// month 从本月 1 号开始，all 从最早一条聚合记录开始（没有则回退 30 天）。
// custom 接受 caller 给的日期，倒置时自动纠正、结束日超过今天时钳到今天。
func NormalizeReportRange(kind string, startDate, endDate string, firstDay string, loc *time.Location) ReportRange {
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	day := func(s string) (time.Time, bool) {
		t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s), loc)
		return t, err == nil
	}
	switch strings.TrimSpace(kind) {
	case "today":
		return ReportRange{today, today, "今天"}
	case "yesterday":
		y := today.AddDate(0, 0, -1)
		return ReportRange{y, y, "昨天"}
	case "7", "7d":
		return ReportRange{today.AddDate(0, 0, -6), today, "近 7 天"}
	case "30", "30d":
		return ReportRange{today.AddDate(0, 0, -29), today, "近 30 天"}
	case "month":
		return ReportRange{time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc), today, "本月"}
	case "custom":
		s, sok := day(startDate)
		e, eok := day(endDate)
		if !sok {
			s = today.AddDate(0, 0, -6)
		}
		if !eok {
			e = today
		}
		if s.After(e) {
			s, e = e, s
		}
		if e.After(today) {
			e = today
		}
		label := s.Format("2006-01-02")
		if !s.Equal(e) {
			label += " ~ " + e.Format("2006-01-02")
		}
		return ReportRange{s, e, label}
	default: // all
		if firstDay != "" {
			if t, ok := day(firstDay); ok && !t.After(today) {
				return ReportRange{t, today, "全部"}
			}
		}
		return ReportRange{today.AddDate(0, 0, -29), today, "全部"}
	}
}

// ReportSummary 是报表接口返回的整包数据。
type ReportSummary struct {
	Range     string         `json:"range"`
	StartDate string         `json:"startDate"`
	EndDate   string         `json:"endDate"`
	Label     string         `json:"label"`
	Overview  ReportTotals   `json:"overview"`
	Models    []ModelUsage   `json:"models"`
	Accounts  []AccountUsage `json:"accounts"`
	APIs      []APIUsage     `json:"apis"`
	Trend     []DailyStat    `json:"trend"`
	// TrendModels / TrendAPIs 是趋势图按维度下钻用的逐日分解，
	// 前端切到「模型 / 协议」维度、或只选某一个实体时直接读这里。
	TrendModels   []DaySlice  `json:"trendModels"`
	TrendAPIs     []DaySlice  `json:"trendApis"`
	TrendAccounts []DaySlice  `json:"trendAccounts"`
	Heatmap       []DailyStat `json:"heatmap"`
	Latency       LatencyStat `json:"latency"`
}

// ReportTotals 是区间总量。
type ReportTotals struct {
	Requests     int64   `json:"requests"`
	Successful   int64   `json:"successful"`
	Tokens       int64   `json:"tokens"`
	PromptTokens int64   `json:"promptTokens"`
	Completion   int64   `json:"completionTokens"`
	ActiveDays   int     `json:"activeDays"`
	Streak       int     `json:"streak"`
	SuccessRate  float64 `json:"successRate"`
	TopModel     string  `json:"topModel"`
	TopModelTok  int64   `json:"topModelTokens"`
}

// ModelUsage 是区间内某模型的汇总。
type ModelUsage struct {
	Model        string  `json:"model"`
	Requests     int64   `json:"requests"`
	Tokens       int64   `json:"tokens"`
	PromptTokens int64   `json:"promptTokens"`
	Completion   int64   `json:"completionTokens"`
	Share        float64 `json:"share"` // 占区间总 token 的比例（0-100）
}

// AccountUsage 是区间内某账号的汇总（账号名从 accounts 表补）。
type AccountUsage struct {
	ID           string  `json:"id"`
	UID          string  `json:"uid"`
	Name         string  `json:"name"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failures     int64   `json:"failures"`
	Tokens       int64   `json:"tokens"`
	PromptTokens int64   `json:"promptTokens"`
	Completion   int64   `json:"completionTokens"`
	Share        float64 `json:"share"`
	SuccessRate  float64 `json:"successRate"`
}

// APIUsage 是区间内某把 API Key 的汇总。流水里现在不存 Key 身份，
// 所以这里按出站协议（openai / anthropic / responses）统计——面板仍叫「协议统计」，
// 数值全都来自真实流水，不编造。
type APIUsage struct {
	API          string  `json:"api"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failures     int64   `json:"failures"`
	Tokens       int64   `json:"tokens"`
	PromptTokens int64   `json:"promptTokens"`
	Completion   int64   `json:"completionTokens"`
	Share        float64 `json:"share"`
	SuccessRate  float64 `json:"successRate"`
}

// LatencyStat 是区间内的耗时分布（从 usage_logs 直接算）。
type LatencyStat struct {
	AvgMS       int64 `json:"avgMs"`
	P50MS       int64 `json:"p50Ms"`
	P95MS       int64 `json:"p95Ms"`
	MaxMS       int64 `json:"maxMs"`
	TokenPerSec int64 `json:"tokenPerSec"`
}

// BackfillDailyIfEmpty 在日聚合表为空时，从现有 usage_logs 补算一遍。
//
// 老版本升级上来时流水已有几十万条，但日聚合表是新建的空表：不补算的话，
// 报表页会显示「没有数据」，用户以为升级把历史弄丢了。只在两表都为空时执行，
// 重复启动不会重复累加。返回补算的流水条数。
func (s *Store) BackfillDailyIfEmpty(ctx context.Context) (int64, error) {
	var have int64
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM request_daily`).Scan(&have); err != nil {
		return 0, err
	}
	if have > 0 {
		return 0, nil
	}
	var minID, maxID int64
	if err := s.queryRow(ctx,
		`SELECT COALESCE(MIN(id),0), COALESCE(MAX(id),0) FROM usage_logs`).Scan(&minID, &maxID); err != nil {
		return 0, err
	}
	if maxID == 0 {
		return 0, nil
	}
	const pageSize = 5000
	var total int64
	for cur := minID; cur <= maxID; {
		rows, err := s.query(ctx,
			`SELECT id, ts, account_id, model, api, status, prompt_tokens, completion_tokens,
				total_tokens, ms, error FROM usage_logs WHERE id >= ? AND id < ? ORDER BY id LIMIT ?`,
			cur, maxID+1, pageSize)
		if err != nil {
			return total, err
		}
		batch := make([]UsageLog, 0, pageSize)
		var last int64
		for rows.Next() {
			var u UsageLog
			if err := rows.Scan(&last, &u.TS, &u.AccountID, &u.Model, &u.API, &u.Status,
				&u.PromptTokens, &u.CompletionTokens, &u.TotalTokens, &u.MS, &u.Error); err != nil {
				rows.Close()
				return total, err
			}
			batch = append(batch, u)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return total, err
		}
		if len(batch) == 0 {
			break
		}
		s.foldDaily(ctx, batch)
		total += int64(len(batch))
		cur = last + 1
	}
	return total, nil
}

// FirstDailyDay 返回日聚合里最早的一天，供 "all" 区间使用。
func (s *Store) FirstDailyDay(ctx context.Context) string {
	var day string
	_ = s.queryRow(ctx, `SELECT day FROM request_daily ORDER BY day ASC LIMIT 1`).Scan(&day)
	return day
}

// LoadDaily 读取区间内的日汇总，零碎缺失的日期不补齐（由调用方按需补零）。
func (s *Store) LoadDaily(ctx context.Context, start, end string) ([]DailyStat, error) {
	rows, err := s.query(ctx,
		`SELECT day, requests, successful, tokens, prompt_tokens, completion_tokens FROM request_daily
		 WHERE day >= ? AND day <= ? ORDER BY day ASC`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyStat
	for rows.Next() {
		var d DailyStat
		if err := rows.Scan(&d.Day, &d.Requests, &d.Successful, &d.Tokens,
			&d.PromptTokens, &d.Completion); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LoadDailyModels 读取区间内按天×模型的用量。
func (s *Store) LoadDailyModels(ctx context.Context, start, end string) ([]DaySlice, error) {
	return s.loadDaySlices(ctx, "request_daily_model", "model", start, end)
}

// LoadDailyAPIs 读取区间内按天×出站协议的用量。
func (s *Store) LoadDailyAPIs(ctx context.Context, start, end string) ([]DaySlice, error) {
	return s.loadDaySlices(ctx, "request_daily_api", "api", start, end)
}

// loadDaySlices 是两张同构分解表的公共读取。表名与列名只来自内部常量，不含用户输入。
func (s *Store) loadDaySlices(ctx context.Context, table, keyCol, start, end string) ([]DaySlice, error) {
	rows, err := s.query(ctx,
		`SELECT day, `+keyCol+`, requests, tokens FROM `+table+`
		 WHERE day >= ? AND day <= ? ORDER BY day ASC`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DaySlice
	for rows.Next() {
		var m DaySlice
		if err := rows.Scan(&m.Day, &m.Key, &m.Requests, &m.Tokens); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LoadHeatmap 读取最近一年的日汇总（热力图固定 365 格，不随区间变化）。
func (s *Store) LoadHeatmap(ctx context.Context, since string) ([]DailyStat, error) {
	rows, err := s.query(ctx,
		`SELECT day, requests, successful, tokens, prompt_tokens, completion_tokens FROM request_daily
		 WHERE day >= ? ORDER BY day ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyStat
	for rows.Next() {
		var d DailyStat
		if err := rows.Scan(&d.Day, &d.Requests, &d.Successful, &d.Tokens,
			&d.PromptTokens, &d.Completion); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LoadAccountUsage 按账号汇总区间内的流水。账号名从 accounts 表补，
// 账号已删除时回落为空，面板显示 UID。
func (s *Store) LoadAccountUsage(ctx context.Context, sinceMS, untilMS int64) ([]AccountUsage, error) {
	// 客户端取消（499）不计入请求数：它不是这个号处理过的任务，
	// 留在分母里会把这个号的成功率压低，而号本身完全健康。
	q := `SELECT u.account_id, COALESCE(a.uid, ''), COALESCE(a.name, ''),
			` + s.dialect.sumInt(canceledExcludeCond) + `,
			` + s.dialect.sumInt("CASE WHEN u.status >= 200 AND u.status < 400 THEN 1 ELSE 0 END") + `,
			` + s.dialect.sumInt("u.total_tokens") + `,
			` + s.dialect.sumInt("u.prompt_tokens") + `,
			` + s.dialect.sumInt("u.completion_tokens") + `
		FROM usage_logs u LEFT JOIN accounts a ON a.id = u.account_id
		WHERE u.ts >= ? AND u.ts < ? AND ` + canceledWhere + `
		GROUP BY u.account_id, a.uid, a.name`
	rows, err := s.query(ctx, q, sinceMS, untilMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountUsage
	var total int64
	for rows.Next() {
		var a AccountUsage
		if err := rows.Scan(&a.ID, &a.UID, &a.Name, &a.Requests, &a.Success, &a.Tokens,
			&a.PromptTokens, &a.Completion); err != nil {
			return nil, err
		}
		total += a.Tokens
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		a := &out[i]
		a.Failures = a.Requests - a.Success
		if a.Requests > 0 {
			a.SuccessRate = float64(a.Success) / float64(a.Requests) * 100
		}
		if total > 0 {
			a.Share = float64(a.Tokens) / float64(total) * 100
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Requests > out[j].Requests
	})
	return out, nil
}

// LoadDailyAccounts 读取区间内按天×账号的用量（从流水实时聚合）。
//
// 账号维度没有独立的日聚合表：账号数远少于请求数，且需要在趋势图里下钻到单个账号，
// 直接按区间扫一遍 usage_logs 的 ts 索引即可，代价可控。
func (s *Store) LoadDailyAccounts(ctx context.Context, sinceMS, untilMS int64) ([]DaySlice, error) {
	// SQL 里按「天」分组要依赖方言各自的日期函数，直接用 Go 归并更稳妥。
	rows, err := s.query(ctx,
		`SELECT u.account_id, u.ts, u.total_tokens FROM usage_logs u
		 WHERE u.ts >= ? AND u.ts < ? AND `+canceledWhere, sinceMS, untilMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	loc := dailyLocation()
	type key struct{ day, acc string }
	agg := map[key]*DaySlice{}
	for rows.Next() {
		var acc string
		var ts, tok int64
		if err := rows.Scan(&acc, &ts, &tok); err != nil {
			return nil, err
		}
		day := time.UnixMilli(ts).In(loc).Format("2006-01-02")
		k := key{day, acc}
		d := agg[k]
		if d == nil {
			d = &DaySlice{Day: day, Key: acc}
			agg[k] = d
		}
		d.Requests++
		d.Tokens += tok
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]DaySlice, 0, len(agg))
	for _, d := range agg {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// LoadAPIUsage 按出站协议汇总区间内的流水。
func (s *Store) LoadAPIUsage(ctx context.Context, sinceMS, untilMS int64) ([]APIUsage, error) {
	q := `SELECT u.api, ` + s.dialect.sumInt(canceledExcludeCond) + `,
			` + s.dialect.sumInt("CASE WHEN u.status >= 200 AND u.status < 400 THEN 1 ELSE 0 END") + `,
			` + s.dialect.sumInt("u.total_tokens") + `,
			` + s.dialect.sumInt("u.prompt_tokens") + `,
			` + s.dialect.sumInt("u.completion_tokens") + `
		FROM usage_logs u
		WHERE u.ts >= ? AND u.ts < ? AND ` + canceledWhere + `
		GROUP BY u.api`
	rows, err := s.query(ctx, q, sinceMS, untilMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIUsage
	var total int64
	for rows.Next() {
		var a APIUsage
		if err := rows.Scan(&a.API, &a.Requests, &a.Success, &a.Tokens,
			&a.PromptTokens, &a.Completion); err != nil {
			return nil, err
		}
		total += a.Tokens
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		a := &out[i]
		a.Failures = a.Requests - a.Success
		if a.Requests > 0 {
			a.SuccessRate = float64(a.Success) / float64(a.Requests) * 100
		}
		if total > 0 {
			a.Share = float64(a.Tokens) / float64(total) * 100
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Requests > out[j].Requests
	})
	return out, nil
}

// LoadLatency 从流水明细里算耗时分布。
//
// P50/P95 用「排序后取位」而不是近似算法：区间内的行数由报表筛选决定，
// 主键索引 (account_id,id) 与 ts 索引都能先用，实际取回的行有限。
func (s *Store) LoadLatency(ctx context.Context, sinceMS, untilMS int64, sampleCap int) (LatencyStat, error) {
	var out LatencyStat
	// 499 是中途被取消的请求，耗时只覆盖到断开为止，混进耗时分会把 P50/P95 拉低。
	// 这里用裸列名（没有 u 别名），所以不能复用 canceledWhere。
	q := `SELECT ms, total_tokens FROM usage_logs
		WHERE ts >= ? AND ts < ? AND status >= 200 AND status <> 499 AND ms > 0 ORDER BY ts DESC LIMIT ?`
	if sampleCap <= 0 {
		sampleCap = 20000
	}
	rows, err := s.query(ctx, q, sinceMS, untilMS, sampleCap)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var latency []int64
	var sumMS, sumTokens int64
	for rows.Next() {
		var ms, tok int64
		if err := rows.Scan(&ms, &tok); err != nil {
			return out, err
		}
		latency = append(latency, ms)
		sumMS += ms
		sumTokens += tok
		if ms > out.MaxMS {
			out.MaxMS = ms
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	n := int64(len(latency))
	if n == 0 {
		return out, nil
	}
	out.AvgMS = sumMS / n
	if sumMS > 0 {
		out.TokenPerSec = sumTokens * 1000 / sumMS
	}
	sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
	out.P50MS = latency[percentileIndex(n, 50)]
	out.P95MS = latency[percentileIndex(n, 95)]
	return out, nil
}

// percentileIndex 返回升序数组中第 p 百分位的下标（最近秩）。
func percentileIndex(n, p int64) int64 {
	idx := (n*p + 99) / 100 // ceil
	if idx < 1 {
		idx = 1
	}
	if idx > n {
		idx = n
	}
	return idx - 1
}

// ActiveDays 统计区间内有请求的天数。
func ActiveDays(days []DailyStat) int {
	n := 0
	for _, d := range days {
		if d.Requests > 0 {
			n++
		}
	}
	return n
}

// CurrentStreak 从今天往回数连续有请求的天数。
//
// 「今天还没开始」不该把连续天数打断：今天没有请求时，从昨天继续往前数。
func CurrentStreak(byDay map[string]int64, today time.Time, loc *time.Location) int {
	cursor := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	if byDay[cursor.Format("2006-01-02")] == 0 {
		cursor = cursor.AddDate(0, 0, -1)
	}
	streak := 0
	for {
		if byDay[cursor.Format("2006-01-02")] == 0 {
			return streak
		}
		streak++
		cursor = cursor.AddDate(0, 0, -1)
	}
}
