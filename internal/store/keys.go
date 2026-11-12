package store

import (
	"context"
	"crypto/subtle"
	"strings"
	"time"
)

type APIKey struct {
	ID        string
	Name      string
	Prefix    string
	Enabled   bool
	CreatedAt int64
	LastUsed  int64
}

// CreateKey 生成 rc- 前缀的 API Key，明文只在这一刻返回，落库只存哈希。
func (s *Store) CreateKey(ctx context.Context, name string) (*APIKey, string, error) {
	secret := "rc-" + RandomHex(24)
	k := &APIKey{
		ID:        "key-" + RandomHex(6),
		Name:      name,
		Prefix:    secret[:11],
		Enabled:   true,
		CreatedAt: time.Now().UnixMilli(),
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, name, prefix, hash, enabled, created_at, last_used) VALUES (?,?,?,?,1,?,0)`,
		k.ID, k.Name, k.Prefix, KeyHash(secret), k.CreatedAt)
	if err != nil {
		return nil, "", err
	}
	return k, secret, nil
}

func (s *Store) ListKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, prefix, enabled, created_at, last_used FROM api_keys ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		var k APIKey
		var enabled int64
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &enabled, &k.CreatedAt, &k.LastUsed); err != nil {
			return nil, err
		}
		k.Enabled = enabled != 0
		out = append(out, &k)
	}
	return out, rows.Err()
}

func (s *Store) DeleteKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

// VerifyKey 校验数据面 API Key，命中则刷新 last_used。
func (s *Store) VerifyKey(ctx context.Context, plain string) bool {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return false
	}
	h := KeyHash(plain)
	var id string
	var enabled int64
	err := s.db.QueryRowContext(ctx, `SELECT id, enabled FROM api_keys WHERE hash = ?`, h).Scan(&id, &enabled)
	if err != nil || enabled == 0 {
		return false
	}
	// 即便 SQL 已按哈希等值匹配，这里再做一次常量时间比较，防时序侧信道。
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT hash FROM api_keys WHERE id = ?`, id).Scan(&stored); err != nil {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(h)) != 1 {
		return false
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used = ? WHERE id = ?`, time.Now().UnixMilli(), id)
	return true
}

func (s *Store) HasAnyKey(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&n)
	return n > 0, err
}
