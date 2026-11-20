package store

import (
	"context"
	"strings"
	"time"
)

// 事件级别。写库时统一收敛到这三个值，面板按级别上色。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// 事件来源（scope）。一个 scope 对应一条业务链路，面板用来快速缩圈。
const (
	ScopeRelay     = "relay"     // 请求转发：上游错误、换号、超时
	ScopePool      = "pool"      // 号池：冷却、恢复、积分
	ScopeCheckin   = "checkin"   // 自动签到
	ScopeRenew     = "renew"     // 凭证续期
	ScopeUpdate    = "update"    // 版本更新
	ScopeLog       = "logs"      // 日志管理自身
	ScopeAccount   = "account"   // 账号增删、导入导出
	ScopeSystem    = "system"    // 启动、配置、其它
	EventMaxDetail = 8 << 10     // detail 上限 8KB，防止把整段响应体写进库
)

// NormalizeLevel 把随手写的级别收敛到 info/warn/error。
func NormalizeLevel(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case LevelError, "fatal", "panic", "err":
		return LevelError
	case LevelWarn, "warning":
		return LevelWarn
	default:
		return LevelInfo
	}
}

// RuntimeEvent 是一条运行期事件。AccountName/AccountUID 由查询时关联账号表补上，
// 账号被删后回落到 AccountID。
type RuntimeEvent struct {
	ID          int64  `json:"id"`
	TS          int64  `json:"ts"`
	Level       string `json:"level"`
	Scope       string `json:"scope"`
	Component   string `json:"component"`
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	AccountUID  string `json:"accountUid"`
	Message     string `json:"message"`
	Detail      string `json:"detail"`
	Count       int64  `json:"count"`
	LastTS      int64  `json:"lastTs"`
}

// EventFilter 是运行日志的检索条件。时间区间为毫秒时间戳，0 表示不限。
type EventFilter struct {
	SinceMS     int64
	UntilMS     int64
	Level       string // info / warn / error
	Scopes      []string
	AccountID   string
	Query       string
	OnlyProblem bool // 只看 warn + error
	Page        int
	PageSize    int
}

func (f EventFilter) conditions() (string, []any) {
	var conds []string
	var args []any
	if f.SinceMS > 0 {
		conds = append(conds, "e.ts >= ?")
		args = append(args, f.SinceMS)
	}
	if f.UntilMS > 0 {
		conds = append(conds, "e.ts < ?")
		args = append(args, f.UntilMS)
	}
	// 级别筛选按「至少到这个程度」理解：选「警告」时错误也要出现，
	// 否则排查时得在两级之间来回切。
	switch strings.ToLower(strings.TrimSpace(f.Level)) {
	case LevelError:
		conds = append(conds, "e.level = 'error'")
	case LevelWarn:
		conds = append(conds, "e.level IN ('warn','error')")
	case LevelInfo:
		conds = append(conds, "e.level = 'info'")
	}
	if f.OnlyProblem && strings.TrimSpace(f.Level) == "" {
		conds = append(conds, "e.level IN ('warn','error')")
	}
	if len(f.Scopes) > 0 {
		ph := make([]string, 0, len(f.Scopes))
		for _, s := range f.Scopes {
			if s = strings.TrimSpace(s); s != "" {
				ph = append(ph, "?")
				args = append(args, s)
			}
		}
		if len(ph) > 0 {
			conds = append(conds, "e.scope IN ("+strings.Join(ph, ",")+")")
		}
	}
	if f.AccountID != "" {
		conds = append(conds, "e.account_id = ?")
		args = append(args, f.AccountID)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + q + "%"
		conds = append(conds,
			"(e.message LIKE ? OR e.detail LIKE ? OR e.component LIKE ? OR e.scope LIKE ? OR e.account_id LIKE ? OR a.name LIKE ? OR a.uid LIKE ?)")
		args = append(args, like, like, like, like, like, like, like)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// EventSummary 是当前筛选下的计数，面板顶部显示。
type EventSummary struct {
	Total   int64 `json:"total"`
	Failed  int64 `json:"failed"`
	Warning int64 `json:"warning"`
}

// QueryEvents 按条件分页查询运行事件，返回当前页与总数/告警数/错误数。
func (s *Store) QueryEvents(ctx context.Context, f EventFilter) ([]RuntimeEvent, EventSummary, error) {
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
	const from = ` FROM runtime_events e LEFT JOIN accounts a ON a.id = e.account_id`

	var sum EventSummary
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN e.level = 'error' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN e.level = 'warn' THEN 1 ELSE 0 END), 0)`+from+where,
		args...).Scan(&sum.Total, &sum.Failed, &sum.Warning); err != nil {
		return nil, EventSummary{}, err
	}

	sel := `SELECT e.id, e.ts, e.level, e.scope, e.component, e.account_id,
		COALESCE(a.name, ''), COALESCE(a.uid, ''), e.message, e.detail, e.count, e.last_ts` +
		from + where + ` ORDER BY e.id DESC LIMIT ? OFFSET ?`
	pageArgs := append(append([]any{}, args...), f.PageSize, int64(f.Page-1)*int64(f.PageSize))
	rows, err := s.db.QueryContext(ctx, sel, pageArgs...)
	if err != nil {
		return nil, EventSummary{}, err
	}
	defer rows.Close()

	out := make([]RuntimeEvent, 0, f.PageSize)
	for rows.Next() {
		var e RuntimeEvent
		if err := rows.Scan(&e.ID, &e.TS, &e.Level, &e.Scope, &e.Component, &e.AccountID,
			&e.AccountName, &e.AccountUID, &e.Message, &e.Detail, &e.Count, &e.LastTS); err != nil {
			return nil, EventSummary{}, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, EventSummary{}, err
	}
	return out, sum, nil
}

// EventFacets 返回时间窗内出现过的来源清单，用于筛选下拉。
func (s *Store) EventFacets(ctx context.Context, sinceMS int64) ([]string, error) {
	q := `SELECT DISTINCT scope FROM runtime_events WHERE scope <> ''`
	var args []any
	if sinceMS > 0 {
		q += ` AND ts >= ?`
		args = append(args, sinceMS)
	}
	q += ` ORDER BY scope`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// eventMergeWindow 是相同事件的合并窗口。窗口内的重复事件只累加次数，
// 不再新增一行——429 风暴或上游连续 5xx 时，列表不会被同一句话刷屏。
const eventMergeWindow = 10 * time.Minute

// AddEvent 写一条运行事件。detail 超长时截断，避免整段上游响应把库撑大。
//
// 合并规则：同一 (level, scope, component, account_id, message) 在
// eventMergeWindow 内再次出现，就把最近一条的 count+1、last_ts 更新。
func (s *Store) AddEvent(ctx context.Context, e RuntimeEvent) error {
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	if e.Count <= 0 {
		e.Count = 1
	}
	e.Level = NormalizeLevel(e.Level)
	if e.Scope == "" {
		e.Scope = ScopeSystem
	}
	e.Message = strings.TrimSpace(e.Message)
	if e.Message == "" {
		return nil
	}
	if len(e.Message) > 2000 {
		e.Message = string([]rune(e.Message)[:2000])
	}
	if len(e.Detail) > EventMaxDetail {
		e.Detail = string([]rune(e.Detail)[:EventMaxDetail])
	}

	// 先试探合并：命中同一句话且还在窗口内，就只累加次数。
	// 用 UPDATE ... 的 RowsAffected 判断是否命中，避免多一次 SELECT。
	res, err := s.db.ExecContext(ctx,
		`UPDATE runtime_events SET count = count + ?, last_ts = ?, detail = ?
		 WHERE id = (SELECT id FROM runtime_events
			WHERE level = ? AND scope = ? AND component = ? AND account_id = ?
			  AND message = ? AND ts >= ?
			ORDER BY id DESC LIMIT 1)`,
		e.Count, e.TS, e.Detail, e.Level, e.Scope, e.Component, e.AccountID, e.Message,
		e.TS-eventMergeWindow.Milliseconds())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO runtime_events (ts, level, scope, component, account_id, message, detail, count, last_ts)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		e.TS, e.Level, e.Scope, e.Component, e.AccountID, e.Message, e.Detail, e.Count, e.TS)
	return err
}

// PurgeEventsBefore 分批删除 ts 早于 cutoffMS 的运行事件。
func (s *Store) PurgeEventsBefore(ctx context.Context, cutoffMS int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM runtime_events WHERE id IN (
			SELECT id FROM runtime_events WHERE ts < ? ORDER BY id LIMIT ?)`, cutoffMS, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EventStats 返回运行事件的条数与最早一条的时间戳。
func (s *Store) EventStats(ctx context.Context) (count, earliest int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(ts), 0) FROM runtime_events`).Scan(&count, &earliest)
	return count, earliest, err
}
