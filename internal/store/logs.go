package store

import (
	"context"
	"time"
)

type CheckinRecord struct {
	AccountID string
	Day       string
	Action    string
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO checkin_history (account_id, day, action, ok, already, credit, message, at) VALUES (?,?,?,?,?,?,?,?)`,
		r.AccountID, r.Day, r.Action, ok, already, r.Credit, r.Message, r.At)
	return err
}

// CheckinFailCount 返回某账号某日某动作的连续失败次数，用于当日熔断。
func (s *Store) CheckinFailCount(ctx context.Context, accountID, day, action string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkin_history WHERE account_id = ? AND day = ? AND action = ? AND ok = 0`,
		accountID, day, action).Scan(&n)
	return n, err
}

func (s *Store) CheckinDoneToday(ctx context.Context, accountID, day string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
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

func (s *Store) AddUsage(ctx context.Context, u UsageLog) error {
	if u.TS == 0 {
		u.TS = time.Now().UnixMilli()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_logs (ts, account_id, model, api, status, prompt_tokens, completion_tokens, total_tokens, ms, error)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		u.TS, u.AccountID, u.Model, u.API, u.Status, u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.MS, u.Error)
	return err
}

func (s *Store) RecentUsage(ctx context.Context, limit int) ([]UsageLog, error) {
	rows, err := s.db.QueryContext(ctx,
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

func (s *Store) GetSetting(ctx context.Context, key, def string) string {
	var v string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return def
	}
	if v == "" {
		return def
	}
	return v
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?,?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
