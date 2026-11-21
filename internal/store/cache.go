package store

import (
	"context"
	"sync"
	"time"
)

// keyCache 缓存「hash → Key ID」的校验命中结果。
//
// 只存命中：miss 不缓存，新建的 Key 立刻可用。
// 删除 Key 时通过 invalidateKey 主动失效。
const (
	keyCacheTTL    = 30 * time.Second
	keyTouchGap    = 60 * time.Second
	accountSnapTTL = 1 * time.Second
)

// KV 是跨实例缓存的最小接口，由 internal/cache 实现。
// 不注入时只用进程内缓存，行为与单机一致。
type KV interface {
	Get(ctx context.Context, key string) (string, bool)
	Set(ctx context.Context, key, val string, ttl time.Duration)
	Del(ctx context.Context, key string)
}

// SetKV 注入跨实例缓存。传 nil 关闭。
func (s *Store) SetKV(kv KV) {
	s.keys.mu.Lock()
	s.keys.kv = kv
	s.keys.mu.Unlock()
}

// keyCacheName 是 Redis 里 Key 校验结果的键名前缀。
func keyCacheName(hash string) string { return "key:" + hash }

type keyCacheEntry struct {
	id string
	at time.Time
}

type keyCacheState struct {
	mu      sync.Mutex
	hits    map[string]keyCacheEntry
	touched map[string]time.Time
	kv      KV
}

func (s *Store) cacheKey(ctx context.Context, hash, id string) {
	s.keys.mu.Lock()
	s.keys.hits[hash] = keyCacheEntry{id: id, at: time.Now()}
	kv := s.keys.kv
	s.keys.mu.Unlock()
	if kv != nil {
		kv.Set(ctx, keyCacheName(hash), id, keyCacheTTL)
	}
}

func (s *Store) cachedKey(ctx context.Context, hash string) (string, bool) {
	s.keys.mu.Lock()
	e, ok := s.keys.hits[hash]
	kv := s.keys.kv
	if ok && time.Since(e.at) > keyCacheTTL {
		delete(s.keys.hits, hash)
		ok = false
	}
	s.keys.mu.Unlock()
	if ok {
		return e.id, true
	}
	// 本进程没有，再问一次共享缓存：另一个副本刚校验过。
	if kv != nil {
		if id, hit := kv.Get(ctx, keyCacheName(hash)); hit && id != "" {
			s.keys.mu.Lock()
			s.keys.hits[hash] = keyCacheEntry{id: id, at: time.Now()}
			s.keys.mu.Unlock()
			return id, true
		}
	}
	return "", false
}

func (s *Store) invalidateKey(hash string) {
	s.keys.mu.Lock()
	delete(s.keys.hits, hash)
	delete(s.keys.touched, hash)
	kv := s.keys.kv
	s.keys.mu.Unlock()
	if kv != nil {
		kv.Del(context.Background(), keyCacheName(hash))
	}
}

// markKeyUsed 返回 true 表示这次应该真正写库。
func (s *Store) markKeyUsed(hash string) bool {
	s.keys.mu.Lock()
	defer s.keys.mu.Unlock()
	now := time.Now()
	if last, ok := s.keys.touched[hash]; ok && now.Sub(last) < keyTouchGap {
		return false
	}
	// 顺手清理过期条目，避免 map 无限增长。
	for h, t := range s.keys.hits {
		if now.Sub(t.at) > keyCacheTTL {
			delete(s.keys.hits, h)
		}
	}
	s.keys.touched[hash] = now
	return true
}

// accountSnapshot 是 ListAccounts 的进程内短缓存。
//
// 注意必须缓存**深拷贝**：Pool.Pick 会写 LastUsed、签到会写 Meta、
// 余额同步会改 Credits。如果共享同一批指针，这些写会污染缓存并产生数据竞争。
type accountSnapshot struct {
	mu    sync.Mutex
	items []*Account
	at    time.Time
	valid bool
}

// invalidateAccounts 清空快照。所有账号写操作成功后调用。
func (s *Store) invalidateAccounts() {
	s.snap.mu.Lock()
	s.snap.valid = false
	s.snap.items = nil
	s.snap.mu.Unlock()
}

func cloneAccounts(in []*Account) []*Account {
	out := make([]*Account, 0, len(in))
	for _, a := range in {
		if a == nil {
			out = append(out, nil)
			continue
		}
		c := *a
		if a.Meta != nil {
			c.Meta = make(map[string]any, len(a.Meta))
			for k, v := range a.Meta {
				c.Meta[k] = v
			}
		}
		out = append(out, &c)
	}
	return out
}

// listAccountsCached 返回快照；未命中时调 listAccountsDB 并缓存深拷贝。
func (s *Store) listAccountsCached(ctx context.Context) ([]*Account, error) {
	s.snap.mu.Lock()
	if s.snap.valid && time.Since(s.snap.at) < accountSnapTTL {
		out := cloneAccounts(s.snap.items)
		s.snap.mu.Unlock()
		return out, nil
	}
	s.snap.mu.Unlock()

	items, err := s.listAccountsDB(ctx)
	if err != nil {
		return nil, err
	}

	s.snap.mu.Lock()
	s.snap.items = cloneAccounts(items)
	s.snap.at = time.Now()
	s.snap.valid = true
	s.snap.mu.Unlock()

	return items, nil
}
