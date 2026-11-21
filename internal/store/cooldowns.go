package store

import (
	"context"
	"strings"
)

// ModelCooldown 是一条「账号 × 模型」的失败链。
type ModelCooldown struct {
	AccountID string
	Model     string
	Until     int64
	Reason    string
	Hits      int64
}

func (s *Store) ListModelCooldowns(ctx context.Context) ([]ModelCooldown, error) {
	rows, err := s.query(ctx, `SELECT account_id, model, until, reason, hits FROM model_cooldowns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelCooldown
	for rows.Next() {
		var c ModelCooldown
		if err := rows.Scan(&c.AccountID, &c.Model, &c.Until, &c.Reason, &c.Hits); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UpsertModelCooldown(ctx context.Context, c ModelCooldown) error {
	_, err := s.exec(ctx,
		`INSERT INTO model_cooldowns (account_id, model, until, reason, hits) VALUES (?,?,?,?,?)
		 ON CONFLICT(account_id, model) DO UPDATE SET until=excluded.until, reason=excluded.reason, hits=excluded.hits`,
		c.AccountID, strings.ToLower(c.Model), c.Until, c.Reason, c.Hits)
	return err
}

func (s *Store) DeleteModelCooldown(ctx context.Context, accountID, model string) error {
	_, err := s.exec(ctx, `DELETE FROM model_cooldowns WHERE account_id = ? AND model = ?`,
		accountID, strings.ToLower(model))
	return err
}

// AccountCooldown 是一条账号级失败链：上游处罚的是整个账号（200108 账号异常、
// 403 风控），换模型绕不过去，所以按账号记命中次数。
type AccountCooldown struct {
	AccountID string
	Until     int64
	Reason    string
	Hits      int64
}

func (s *Store) ListAccountCooldowns(ctx context.Context) ([]AccountCooldown, error) {
	rows, err := s.query(ctx, `SELECT account_id, until, reason, hits FROM account_cooldowns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountCooldown
	for rows.Next() {
		var c AccountCooldown
		if err := rows.Scan(&c.AccountID, &c.Until, &c.Reason, &c.Hits); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UpsertAccountCooldown(ctx context.Context, c AccountCooldown) error {
	_, err := s.exec(ctx,
		`INSERT INTO account_cooldowns (account_id, until, reason, hits) VALUES (?,?,?,?)
		 ON CONFLICT(account_id) DO UPDATE SET until=excluded.until, reason=excluded.reason, hits=excluded.hits`,
		c.AccountID, c.Until, c.Reason, c.Hits)
	return err
}

func (s *Store) DeleteAccountCooldown(ctx context.Context, accountID string) error {
	_, err := s.exec(ctx, `DELETE FROM account_cooldowns WHERE account_id = ?`, accountID)
	return err
}
