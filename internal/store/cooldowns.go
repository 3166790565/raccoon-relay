package store

import (
	"context"
	"strings"
)

type ModelCooldown struct {
	AccountID string
	Model     string
	Until     int64
	Reason    string
	Hits      int64
}

func (s *Store) ListModelCooldowns(ctx context.Context) ([]ModelCooldown, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, model, until, reason, hits FROM model_cooldowns`)
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_cooldowns (account_id, model, until, reason, hits) VALUES (?,?,?,?,?)
		 ON CONFLICT(account_id, model) DO UPDATE SET until=excluded.until, reason=excluded.reason, hits=excluded.hits`,
		c.AccountID, strings.ToLower(c.Model), c.Until, c.Reason, c.Hits)
	return err
}

func (s *Store) DeleteModelCooldown(ctx context.Context, accountID, model string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_cooldowns WHERE account_id = ? AND model = ?`,
		accountID, strings.ToLower(model))
	return err
}
