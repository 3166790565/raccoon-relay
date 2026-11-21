package store

import (
	"context"
	"strings"
	"time"
)

type CheckinRecord struct {
	AccountID string
	Day       string
	Action    string
	Source    string // auto / manual / onboard，见 CheckinSourceXxx
	OK        bool
	Already   bool
	Credit    int64
	Message   string
	At        int64
}

func (s *Store) AddCheckin(ctx context.Context, r CheckinRecord) error {
	ok, already := 0, 0
	if r.OK {
		ok = 1
	}
	if r.Already {
		already = 1
	}
	if r.At == 0 {
		r.At = time.Now().UnixMilli()
	}
	_, err := s.exec(ctx,
		`INSERT INTO checkin_history (account_id, day, action, source, ok, already, credit, message, at) VALUES (?,?,?,?,?,?,?,?,?)`,
		r.AccountID, r.Day, r.Action, r.Source, ok, already, r.Credit, r.Message, r.At)
	return err
}

// CheckinFailCount 返回某账号某日某动作的连续失败次数，用于当日熔断。
func (s *Store) CheckinFailCount(ctx context.Context, accountID, day, action string) (int, error) {
	var n int
	err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM checkin_history WHERE account_id = ? AND day = ? AND action = ? AND ok = 0`,
		accountID, day, action).Scan(&n)
	return n, err
}

func (s *Store) CheckinDoneToday(ctx context.Context, accountID, day string) (bool, error) {
	var n int
	err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM checkin_history WHERE account_id = ? AND day = ? AND ok = 1`,
		accountID, day).Scan(&n)
	return n > 0, err
}

type UsageLog struct {
	TS               int64
	AccountID        string
	Model            string
	API              string
	Status           int
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	MS               int64
	Error            string
}

// UsageRow 是调用日志列表的一行，带上账号名方便面板直接展示。
// 账号可能已被删除，此时 Name/UID 为空，前端回落到 AccountID。
type UsageRow struct {
	ID               int64  `json:"id"`
	TS               int64  `json:"ts"`
	AccountID        string `json:"accountId"`
	AccountName      string `json:"accountName"`
	AccountUID       string `json:"accountUid"`
	Model            string `json:"model"`
	API              string `json:"api"`
	Status           int    `json:"status"`
	PromptTokens     int64  `json:"promptTokens"`
	CompletionTokens int64  `json:"completionTokens"`
	TotalTokens      int64  `json:"totalTokens"`
	MS               int64  `json:"ms"`
	Error            string `json:"error"`
}

// UsageFilter 是日志列表的筛选与分页条件。时间区间为毫秒时间戳，0 表示不限。
type UsageFilter struct {
	SinceMS   int64
	UntilMS   int64
	Status    string // ok / fail / 4xx / 5xx
	AccountID string
	Model     string
	API       string
	Query     string // 模糊搜模型、错误、接口、账号名/uid
	Page      int
	PageSize  int
}

func (f UsageFilter) conditions(d dialect) (string, []any) {
	var conds []string
	var args []any
	if f.SinceMS > 0 {
		conds = append(conds, "u.ts >= ?")
		args = append(args, f.SinceMS)
	}
	if f.UntilMS > 0 {
		conds = append(conds, "u.ts < ?")
		args = append(args, f.UntilMS)
	}
	switch f.Status {
	case "ok":
		conds = append(conds, "u.status < 400")
	case "fail":
		conds = append(conds, "u.status >= 400")
	case "4xx":
		conds = append(conds, "u.status >= 400 AND u.status < 500")
	case "5xx":
		conds = append(conds, "u.status >= 500")
	}
	if f.AccountID != "" {
		conds = append(conds, "u.account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.Model != "" {
		conds = append(conds, "u.model = ?")
		args = append(args, f.Model)
	}
	if f.API != "" {
		conds = append(conds, "u.api = ?")
		args = append(args, f.API)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := escapeLike(q)
		op := d.likeOp()
		conds = append(conds,
			"(u.model "+op+" ? OR u.error "+op+" ? OR u.api "+op+" ? OR a.name "+op+" ? OR a.uid "+op+" ? OR u.account_id "+op+" ?)")
		args = append(args, like, like, like, like, like, like)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// QueryUsage 按条件分页查询调用流水，返回当前页、总条数与其中的失败条数。
// 总条数与失败数在同一次 COUNT 里算，避免面板为了显示「N 条 · M 失败」再扫一遍。
func (s *Store) QueryUsage(ctx context.Context, f UsageFilter) ([]UsageRow, int64, int64, error) {
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 50
	}
	if f.PageSize > 500 {
		f.PageSize = 500
	}
	where, args := f.conditions(s.dialect)
	const from = ` FROM usage_logs u LEFT JOIN accounts a ON a.id = u.account_id`

	var total, failed int64
	if err := s.queryRow(ctx,
		`SELECT COUNT(*), `+s.dialect.sumInt("CASE WHEN u.status >= 400 THEN 1 ELSE 0 END")+from+where,
		args...).Scan(&total, &failed); err != nil {
		return nil, 0, 0, err
	}

	sel := `SELECT u.id, u.ts, u.account_id, COALESCE(a.name, ''), COALESCE(a.uid, ''),
		u.model, u.api, u.status, u.prompt_tokens, u.completion_tokens, u.total_tokens, u.ms, u.error` +
		from + where + ` ORDER BY u.id DESC LIMIT ? OFFSET ?`
	pageArgs := append(append([]any{}, args...), f.PageSize, int64(f.Page-1)*int64(f.PageSize))
	rows, err := s.query(ctx, sel, pageArgs...)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()

	out := make([]UsageRow, 0, f.PageSize)
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.ID, &u.TS, &u.AccountID, &u.AccountName, &u.AccountUID,
			&u.Model, &u.API, &u.Status, &u.PromptTokens, &u.CompletionTokens,
			&u.TotalTokens, &u.MS, &u.Error); err != nil {
			return nil, 0, 0, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	return out, total, failed, nil
}

// UsageFacets 返回筛选下拉用的模型与接口清单。只统计窗口内的数据，
// 走 idx_usage_ts 索引，避免每次打开日志页都全表 DISTINCT。
func (s *Store) UsageFacets(ctx context.Context, sinceMS int64) (models, apis []string, err error) {
	q := `SELECT DISTINCT model FROM usage_logs WHERE model <> ''`
	var args []any
	if sinceMS > 0 {
		q += ` AND ts >= ?`
		args = append(args, sinceMS)
	}
	q += ` ORDER BY model LIMIT 300`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, nil, err
		}
		models = append(models, v)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	q = `SELECT DISTINCT api FROM usage_logs WHERE api <> ''`
	args = args[:0]
	if sinceMS > 0 {
		q += ` AND ts >= ?`
		args = append(args, sinceMS)
	}
	q += ` ORDER BY api`
	rows2, err := s.query(ctx, q, args...)
	if err != nil {
		return models, nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var v string
		if err := rows2.Scan(&v); err != nil {
			return models, nil, err
		}
		apis = append(apis, v)
	}
	return models, apis, rows2.Err()
}

func (s *Store) AddUsage(ctx context.Context, u UsageLog) error {
	if u.TS == 0 {
		u.TS = time.Now().UnixMilli()
	}
	_, err := s.exec(ctx,
		`INSERT INTO usage_logs (ts, account_id, model, api, status, prompt_tokens, completion_tokens, total_tokens, ms, error)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		u.TS, u.AccountID, u.Model, u.API, u.Status, u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.MS, u.Error)
	return err
}

func (s *Store) RecentUsage(ctx context.Context, limit int) ([]UsageLog, error) {
	rows, err := s.query(ctx,
		`SELECT ts, account_id, model, api, status, prompt_tokens, completion_tokens, total_tokens, ms, error
		 FROM usage_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageLog
	for rows.Next() {
		var u UsageLog
		if err := rows.Scan(&u.TS, &u.AccountID, &u.Model, &u.API, &u.Status,
			&u.PromptTokens, &u.CompletionTokens, &u.TotalTokens, &u.MS, &u.Error); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// PurgeUsageBefore 删除 ts 早于 cutoffMS 的调用流水。
//
// 分批删除（每次最多 limit 条）而不是一条 DELETE 全清：
// SQLite 单连接写，一次性删除几十万行会长时间占住写锁，把正在服务的请求卡住。
func (s *Store) PurgeUsageBefore(ctx context.Context, cutoffMS int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := s.exec(ctx,
		`DELETE FROM usage_logs WHERE id IN (
			SELECT id FROM usage_logs WHERE ts < ? ORDER BY id LIMIT ?)`, cutoffMS, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeCheckinBefore 删除 at 早于 cutoffMS 的签到记录。
func (s *Store) PurgeCheckinBefore(ctx context.Context, cutoffMS int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := s.exec(ctx,
		`DELETE FROM checkin_history WHERE id IN (
			SELECT id FROM checkin_history WHERE at < ? ORDER BY id LIMIT ?)`, cutoffMS, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UsageStats 返回调用流水的条数与最早一条的时间戳（空表时 earliest 为 0）。
func (s *Store) UsageStats(ctx context.Context) (count, earliest int64, err error) {
	err = s.queryRow(ctx,
		`SELECT COUNT(*), COALESCE(MIN(ts), 0) FROM usage_logs`).Scan(&count, &earliest)
	return count, earliest, err
}

// CheckinStats 返回签到记录的条数与最早一条的时间戳。
func (s *Store) CheckinStats(ctx context.Context) (count, earliest int64, err error) {
	err = s.queryRow(ctx,
		`SELECT COUNT(*), COALESCE(MIN(at), 0) FROM checkin_history`).Scan(&count, &earliest)
	return count, earliest, err
}

func (s *Store) GetSetting(ctx context.Context, key, def string) string {
	var v string
	if err := s.queryRow(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return def
	}
	if v == "" {
		return def
	}
	return v
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.exec(ctx,
		`INSERT INTO settings (key, value) VALUES (?,?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
