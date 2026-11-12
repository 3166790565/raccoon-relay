package pool

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 冷却分级与桌面号池一致：
//   402 积分耗尽 → exhausted 至次日 04:00
//   429 限流     → cooling 60s（有 Retry-After 用绝对值）
//   5xx / 网络   → cooling 10min
//   401 刷新失败 → relogin 永久
//   风控拦截     → cooling 2h
//   其它 4xx     → cooling 60s

type CooldownBook struct {
	st      *store.Store
	enabled bool

	mu       sync.Mutex
	models   map[string]modelCool
	hydrated bool
}

type modelCool struct {
	until int64
	hits  int64
}

func NewCooldownBook(st *store.Store, modelCooldownEnabled bool) *CooldownBook {
	return &CooldownBook{st: st, enabled: modelCooldownEnabled, models: map[string]modelCool{}}
}

func coolKey(accID, model string) string { return strings.ToLower(accID + "∥" + model) }

func (c *CooldownBook) hydrate(ctx context.Context) {
	c.mu.Lock()
	if c.hydrated {
		c.mu.Unlock()
		return
	}
	c.hydrated = true
	c.mu.Unlock()

	rows, err := c.st.ListModelCooldowns(ctx)
	if err != nil {
		return
	}
	c.mu.Lock()
	for _, r := range rows {
		c.models[coolKey(r.AccountID, r.Model)] = modelCool{until: r.Until, hits: r.Hits}
	}
	c.mu.Unlock()
}

func (c *CooldownBook) IsModelCooled(accID, model string) bool {
	if !c.enabled || model == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := coolKey(accID, model)
	m, ok := c.models[k]
	if !ok {
		return false
	}
	if m.until <= time.Now().UnixMilli() {
		delete(c.models, k)
		return false
	}
	return true
}

// CoolModel 记一次模型级失败。连续失败用指数退避，上限 24h（对齐桌面口径）。
func (c *CooldownBook) CoolModel(ctx context.Context, accID, model, reason string, base time.Duration) {
	if !c.enabled || model == "" {
		return
	}
	if base <= 0 {
		base = 5 * time.Minute
	}
	now := time.Now().UnixMilli()
	c.mu.Lock()
	k := coolKey(accID, model)
	prev := c.models[k]
	hits := int64(0)
	if prev.until > now {
		hits = prev.hits + 1
	}
	backoff := base * time.Duration(1<<min64(hits, 4))
	if backoff > 24*time.Hour {
		backoff = 24 * time.Hour
	}
	m := modelCool{until: now + backoff.Milliseconds(), hits: hits}
	c.models[k] = m
	c.mu.Unlock()
	_ = c.st.UpsertModelCooldown(ctx, store.ModelCooldown{
		AccountID: accID, Model: model, Until: m.until, Reason: reason, Hits: hits,
	})
}

func (c *CooldownBook) ClearModel(ctx context.Context, accID, model string) {
	if model == "" {
		return
	}
	c.mu.Lock()
	delete(c.models, coolKey(accID, model))
	c.mu.Unlock()
	_ = c.st.DeleteModelCooldown(ctx, accID, model)
}

// FailKind 是失败分类，由转发层判定后交给号池执行冷却。
type FailKind string

const (
	FailQuota       FailKind = "quota"        // 402
	FailRate        FailKind = "rate"         // 429
	FailServer      FailKind = "server"       // 5xx / 网络
	FailRelogin     FailKind = "relogin"      // 401 且刷新失败
	FailBlocked     FailKind = "blocked"      // 风控
	FailModelScoped FailKind = "model_scoped" // 模型级问题，罚账号×模型
	FailOther       FailKind = "other"
)

// Cool 按分类写账号冷却。
func (c *CooldownBook) Cool(ctx context.Context, accID string, kind FailKind, detail string) {
	now := time.Now().UnixMilli()
	switch kind {
	case FailQuota:
		c.setAccount(ctx, accID, store.StatusExhausted, nextDay4AM(), defaultStr(detail, "积分耗尽"))
	case FailRate:
		c.setAccount(ctx, accID, store.StatusCooling, now+60_000, defaultStr(detail, "上游限流"))
	case FailServer:
		c.setAccount(ctx, accID, store.StatusCooling, now+600_000, defaultStr(detail, "上游异常"))
	case FailRelogin:
		c.setAccount(ctx, accID, store.StatusRelogin, 0, defaultStr(detail, "凭证失效，需重新登录"))
	case FailBlocked:
		c.setAccount(ctx, accID, store.StatusCooling, now+2*3_600_000, defaultStr(detail, "账号被上游风控拦截"))
	default:
		c.setAccount(ctx, accID, store.StatusCooling, now+60_000, detail)
	}
}

// CoolUntil 按绝对时刻冷却，用于对齐 Retry-After。
func (c *CooldownBook) CoolUntil(ctx context.Context, accID string, until int64, detail string) {
	if until <= time.Now().UnixMilli() {
		until = time.Now().UnixMilli() + 1000
	}
	c.setAccount(ctx, accID, store.StatusCooling, until, defaultStr(detail, "上游限流"))
}

func (c *CooldownBook) setAccount(ctx context.Context, accID, status string, until int64, reason string) {
	_ = c.st.UpdateAccount(ctx, accID, store.AccountPatch{
		Status: &status, CoolUntil: &until, CoolReason: &reason,
	})
}

// MarkOnline 成功调用后清冷却与状态（只改确实变化的字段）。
func (c *CooldownBook) MarkOnline(ctx context.Context, accID string) {
	zero := int64(0)
	empty := ""
	online := store.StatusOnline
	_ = c.st.UpdateAccount(ctx, accID, store.AccountPatch{
		Status: &online, CoolUntil: &zero, CoolReason: &empty,
	})
}

// ReviveExpired 把冷却到期的账号复活。定时器每轮调用一次。
func (c *CooldownBook) ReviveExpired(ctx context.Context) {
	accounts, err := c.st.ListAccounts(ctx)
	if err != nil {
		return
	}
	now := time.Now().UnixMilli()
	for _, a := range accounts {
		if a.Status != store.StatusCooling && a.Status != store.StatusExhausted {
			continue
		}
		if a.CoolUntil == 0 || a.CoolUntil > now {
			continue
		}
		st := store.StatusOnline
		zero := int64(0)
		empty := ""
		_ = c.st.UpdateAccount(ctx, a.ID, store.AccountPatch{
			Status: &st, CoolUntil: &zero, CoolReason: &empty,
		})
	}
}

func nextDay4AM() int64 {
	now := time.Now()
	d := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	if !d.After(now) {
		d = d.AddDate(0, 0, 1)
	}
	return d.UnixMilli()
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
