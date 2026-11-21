package store

import (
	"context"
	"database/sql"
	"strings"
)

// 签到触发来源。落库后在签到中心按来源筛选，区分自动任务和人工补签。
const (
	CheckinSourceAuto    = "auto"    // 定时任务
	CheckinSourceManual  = "manual"  // 面板手动
	CheckinSourceOnboard = "onboard" // 新号落地
)

// CheckinRow 是签到记录列表的一行，带上账号名方便面板直接展示。
// 账号被删后 Name/UID 为空，前端回落到 AccountID。
type CheckinRow struct {
	ID          int64  `json:"id"`
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	AccountUID  string `json:"accountUid"`
	Day         string `json:"day"`
	Action      string `json:"action"`
	Source      string `json:"source"`
	OK          bool   `json:"ok"`
	Already     bool   `json:"already"`
	Credit      int64  `json:"credit"`
	Message     string `json:"message"`
	At          int64  `json:"at"`
}

// CheckinFilter 是签到记录的检索条件。时间区间为毫秒时间戳，0 表示不限。
type CheckinFilter struct {
	SinceMS   int64
	UntilMS   int64
	Day       string // 指定某一天（YYYY-MM-DD）
	AccountID string
	Result    string // ok / fail / claimed / already
	Source    string
	Query     string
	Page      int
	PageSize  int
}

func (f CheckinFilter) conditions() (string, []any) {
	var conds []string
	var args []any
	if f.SinceMS > 0 {
		conds = append(conds, "c.at >= ?")
		args = append(args, f.SinceMS)
	}
	if f.UntilMS > 0 {
		conds = append(conds, "c.at < ?")
		args = append(args, f.UntilMS)
	}
	if f.Day != "" {
		conds = append(conds, "c.day = ?")
		args = append(args, f.Day)
	}
	if f.AccountID != "" {
		conds = append(conds, "c.account_id = ?")
		args = append(args, f.AccountID)
	}
	switch f.Result {
	case "ok":
		conds = append(conds, "c.ok = 1")
	case "fail":
		conds = append(conds, "c.ok = 0")
	case "claimed":
		conds = append(conds, "c.ok = 1 AND c.already = 0")
	case "already":
		conds = append(conds, "c.already = 1")
	}
	if f.Source != "" {
		conds = append(conds, "c.source = ?")
		args = append(args, f.Source)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + q + "%"
		conds = append(conds,
			"(c.message LIKE ? OR c.account_id LIKE ? OR a.name LIKE ? OR a.uid LIKE ?)")
		args = append(args, like, like, like, like)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// CheckinSummary 是当前筛选下的计数，面板顶部显示。
type CheckinSummary struct {
	Total   int64 `json:"total"`
	OK      int64 `json:"ok"`
	Failed  int64 `json:"failed"`
	Claimed int64 `json:"claimed"`
	Credit  int64 `json:"credit"`
}

// QueryCheckins 按条件分页查询签到记录，返回当前页与计数摘要。
func (s *Store) QueryCheckins(ctx context.Context, f CheckinFilter) ([]CheckinRow, CheckinSummary, error) {
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 50
	}
	if f.PageSize > 500 {
		f.PageSize = 500
	}
	where, args := f.conditions()
	const from = ` FROM checkin_history c LEFT JOIN accounts a ON a.id = c.account_id`

	var sum CheckinSummary
	if err := s.queryRow(ctx,
		`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN c.ok = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN c.ok = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN c.ok = 1 AND c.already = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN c.ok = 1 THEN c.credit ELSE 0 END), 0)`+from+where,
		args...).Scan(&sum.Total, &sum.OK, &sum.Failed, &sum.Claimed, &sum.Credit); err != nil {
		return nil, CheckinSummary{}, err
	}

	sel := `SELECT c.id, c.account_id, COALESCE(a.name, ''), COALESCE(a.uid, ''),
		c.day, c.action, c.source, c.ok, c.already, c.credit, c.message, c.at` +
		from + where + ` ORDER BY c.id DESC LIMIT ? OFFSET ?`
	pageArgs := append(append([]any{}, args...), f.PageSize, int64(f.Page-1)*int64(f.PageSize))
	rows, err := s.query(ctx, sel, pageArgs...)
	if err != nil {
		return nil, CheckinSummary{}, err
	}
	defer rows.Close()

	out := make([]CheckinRow, 0, f.PageSize)
	for rows.Next() {
		var r CheckinRow
		var ok, already int
		if err := rows.Scan(&r.ID, &r.AccountID, &r.AccountName, &r.AccountUID,
			&r.Day, &r.Action, &r.Source, &ok, &already, &r.Credit, &r.Message, &r.At); err != nil {
			return nil, CheckinSummary{}, err
		}
		r.OK, r.Already = ok != 0, already != 0
		out = append(out, r)
	}
	return out, sum, rows.Err()
}

// CheckinCenterAccount 是签到中心账号列表的一行：账号档案 + 今日最近一次签到结果。
type CheckinCenterAccount struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	UID            string `json:"uid"`
	Note           string `json:"note"`
	Enabled        bool   `json:"enabled"`
	Status         string `json:"status"`
	Credits        int64  `json:"credits"`
	LastCheckinDay string `json:"lastCheckinDay"`
	// 今日最近一条签到记录（没有则为空）
	TodayOK      bool   `json:"todayOk"`
	TodayAlready bool   `json:"todayAlready"`
	TodayMessage string `json:"todayMessage"`
	TodayAt      int64  `json:"todayAt"`
	TodaySource  string `json:"todaySource"`
	HasToday     bool   `json:"hasToday"`
}

// CheckinCenterAccounts 返回全部账号及各自「今日」最近一次签到结果。
// 一次 SQL 取齐，避免面板逐号查询；LEFT JOIN 到今日最新一条记录。
func (s *Store) CheckinCenterAccounts(ctx context.Context, day string) ([]CheckinCenterAccount, error) {
	rows, err := s.query(ctx, `
		SELECT a.id, a.name, a.uid, a.note, a.enabled, a.status, a.credits, a.last_checkin_day,
			c.id, COALESCE(c.ok, 0), COALESCE(c.already, 0), COALESCE(c.message, ''),
			COALESCE(c.at, 0), COALESCE(c.source, '')
		FROM accounts a
		LEFT JOIN checkin_history c ON c.id = (
			SELECT x.id FROM checkin_history x
			WHERE x.account_id = a.id AND x.day = ?
			ORDER BY x.id DESC LIMIT 1)
		ORDER BY a.enabled DESC, a.created_at ASC`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CheckinCenterAccount
	for rows.Next() {
		var a CheckinCenterAccount
		var enabled, ok, already int
		var recID sql.NullInt64
		if err := rows.Scan(&a.ID, &a.Name, &a.UID, &a.Note, &enabled, &a.Status,
			&a.Credits, &a.LastCheckinDay, &recID, &ok, &already, &a.TodayMessage,
			&a.TodayAt, &a.TodaySource); err != nil {
			return nil, err
		}
		a.Enabled = enabled != 0
		a.HasToday = recID.Valid
		a.TodayOK, a.TodayAlready = ok != 0, already != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// CheckinTotal 返回签到记录总数，签到中心「累计记录」用。
func (s *Store) CheckinTotal(ctx context.Context) (int64, error) {
	var n int64
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM checkin_history`).Scan(&n)
	return n, err
}

// CheckinCreditTotal 返回签到累计领到的积分（只累计成功且真正领取的记录）。
func (s *Store) CheckinCreditTotal(ctx context.Context) (int64, error) {
	var n int64
	err := s.queryRow(ctx,
		`SELECT COALESCE(SUM(credit), 0) FROM checkin_history WHERE ok = 1`).Scan(&n)
	return n, err
}
