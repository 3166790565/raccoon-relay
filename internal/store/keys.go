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
	_, err := s.exec(ctx,
		`INSERT INTO api_keys (id, name, prefix, hash, enabled, created_at, last_used) VALUES (?,?,?,?,1,?,0)`,
		k.ID, k.Name, k.Prefix, KeyHash(secret), k.CreatedAt)
	if err != nil {
		return nil, "", err
	}
	return k, secret, nil
}

func (s *Store) ListKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.query(ctx, `SELECT id, name, prefix, enabled, created_at, last_used FROM api_keys ORDER BY created_at ASC`)
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
	// 先取出 hash，删掉后主动失效缓存，避免删完还能用一整个 TTL。
	var hash string
	_ = s.queryRow(ctx, `SELECT hash FROM api_keys WHERE id = ?`, id).Scan(&hash)
	_, err := s.exec(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if hash != "" {
		s.invalidateKey(hash)
	}
	return err
}

// VerifyKey 校验数据面 API Key，命中则（节流地）刷新 last_used。
//
// 每个转发请求都会走这里，所以：
//   - 只查一次库：按 hash 等值（有索引）拿到 id/hash/enabled，
//     再对返回的 hash 做常量时间比较，不需要第二次查询；
//   - 命中结果缓存 30s，且只缓存命中、不缓存 miss——否则新建的 Key
//     要等 TTL 过期才能用；
//   - last_used 每个 Key 最多 60s 写一次，之前是每个请求一次写。
func (s *Store) VerifyKey(ctx context.Context, plain string) bool {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return false
	}
	h := KeyHash(plain)

	if id, ok := s.cachedKey(h); ok {
		// 缓存里存的是已验证过的 id；disabled 的 Key 不会进缓存。
		s.touchKey(ctx, id, h)
		return true
	}

	var id, stored string
	var enabled int64
	err := s.queryRow(ctx, `SELECT id, hash, enabled FROM api_keys WHERE hash = ?`, h).
		Scan(&id, &stored, &enabled)
	if err != nil || enabled == 0 {
		return false
	}
	// 即便 SQL 已按哈希等值匹配，这里再做一次常量时间比较，防时序侧信道。
	if subtle.ConstantTimeCompare([]byte(stored), []byte(h)) != 1 {
		return false
	}
	s.cacheKey(h, id)
	s.touchKey(ctx, id, h)
	return true
}

// touchKey 节流刷新 last_used：同一 Key 60s 内只写一次。
func (s *Store) touchKey(ctx context.Context, id, hash string) {
	if !s.markKeyUsed(hash) {
		return
	}
	_, _ = s.exec(ctx, `UPDATE api_keys SET last_used = ? WHERE id = ?`, time.Now().UnixMilli(), id)
}

func (s *Store) HasAnyKey(ctx context.Context) (bool, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&n)
	return n > 0, err
}
