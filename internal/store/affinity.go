package store

import (
	"context"
	"strings"
)

// ---------- 会话粘性 ----------

// SessionBinding 是一段对话当前绑定的账号。
//
// 为什么要有它：上游风控看的不是单个请求，而是「同一个账号在一段时间里
// 接了多少互不相干的对话」。每轮重新选号会让一段对话在多个账号之间跳，
// 形成明显的第三方特征。绑定之后，同一会话的连续轮次落在同一个号上。
type SessionBinding struct {
	SessionKey string
	AccountID  string
	Model      string
	ExpiresAt  int64
	UpdatedAt  int64
}

// BoundAccount 返回仍然有效的绑定。过期或不存在时返回空串。
func (s *Store) BoundAccount(ctx context.Context, sessionKey string, now int64) (string, bool) {
	if strings.TrimSpace(sessionKey) == "" {
		return "", false
	}
	var accID string
	var expiresAt int64
	err := s.queryRow(ctx,
		`SELECT account_id, expires_at FROM session_affinity WHERE session_key = ?`, sessionKey).
		Scan(&accID, &expiresAt)
	if err != nil || accID == "" || expiresAt <= now {
		return "", false
	}
	return accID, true
}

// BindSession 绑定会话到账号，并滚动续期 TTL。
func (s *Store) BindSession(ctx context.Context, sessionKey, accountID, model string, ttlMS, now int64) error {
	if strings.TrimSpace(sessionKey) == "" || accountID == "" {
		return nil
	}
	_, err := s.exec(ctx,
		`INSERT INTO session_affinity (session_key, account_id, model, expires_at, updated_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(session_key) DO UPDATE SET
		   account_id=excluded.account_id, model=excluded.model,
		   expires_at=excluded.expires_at, updated_at=excluded.updated_at`,
		sessionKey, accountID, model, now+ttlMS, now)
	return err
}

// UnbindSession 解除绑定。绑定的号失败时立刻解绑，避免后续轮次继续撞上它。
func (s *Store) UnbindSession(ctx context.Context, sessionKey string) error {
	if strings.TrimSpace(sessionKey) == "" {
		return nil
	}
	_, err := s.exec(ctx, `DELETE FROM session_affinity WHERE session_key = ?`, sessionKey)
	return err
}

// PruneSessions 清理过期绑定，定时巡检调用。
func (s *Store) PruneSessions(ctx context.Context, now int64) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM session_affinity WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---------- 换号退避门闸 ----------

// AccountBackoff 是一个账号的退避窗口与失败链长度。
//
// 与冷却分开：冷却改的是账号状态（面板上会显示成停用/限流），
// 退避只是调度层暂时跳过它。403 / 5xx / 网络抖动属于上游波动，
// 不该把账号打成坏号，但也不该在失败后立刻再被选中。
type AccountBackoff struct {
	AccountID string
	Until     int64
	Reason    string
	Hits      int64
}

// BackoffOf 返回账号当前的退避窗口；未在退避中时 Until 为 0。
func (s *Store) BackoffOf(ctx context.Context, accountID string) (AccountBackoff, error) {
	var b AccountBackoff
	err := s.queryRow(ctx,
		`SELECT account_id, until, reason, hits FROM account_backoffs WHERE account_id = ?`, accountID).
		Scan(&b.AccountID, &b.Until, &b.Reason, &b.Hits)
	if err != nil {
		return AccountBackoff{AccountID: accountID}, nil
	}
	return b, nil
}

// ListBackoffs 返回全部退避记录，选号层一次读出、按 map 过滤。
func (s *Store) ListBackoffs(ctx context.Context) (map[string]AccountBackoff, error) {
	rows, err := s.query(ctx, `SELECT account_id, until, reason, hits FROM account_backoffs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AccountBackoff{}
	for rows.Next() {
		var b AccountBackoff
		if err := rows.Scan(&b.AccountID, &b.Until, &b.Reason, &b.Hits); err != nil {
			return nil, err
		}
		out[b.AccountID] = b
	}
	return out, rows.Err()
}

// UpsertBackoff 写入退避窗口。
func (s *Store) UpsertBackoff(ctx context.Context, b AccountBackoff) error {
	_, err := s.exec(ctx,
		`INSERT INTO account_backoffs (account_id, until, reason, hits) VALUES (?,?,?,?)
		 ON CONFLICT(account_id) DO UPDATE SET until=excluded.until, reason=excluded.reason, hits=excluded.hits`,
		b.AccountID, b.Until, b.Reason, b.Hits)
	return err
}

// ClearBackoff 成功调用后解除退避并清空失败链。
func (s *Store) ClearBackoff(ctx context.Context, accountID string) error {
	_, err := s.exec(ctx, `DELETE FROM account_backoffs WHERE account_id = ?`, accountID)
	return err
}

// PruneBackoffs 清理早已过期的退避记录（保留一小段，供面板展示最近一次失败）。
func (s *Store) PruneBackoffs(ctx context.Context, before int64) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM account_backoffs WHERE until < ?`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
