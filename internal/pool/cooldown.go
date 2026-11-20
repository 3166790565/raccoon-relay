package pool

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 冷却分级：
//   402 积分耗尽 → exhausted 至次日 04:00
//   429 限流     → 默认只罚「账号 × 模型」，阶梯 3 → 5 → 10 → 20… 分钟；
//                  关闭模型级冷却时才退化为整号 60s
//   5xx / 网络   → cooling 10min
//   401 刷新失败 → relogin 永久
//   风控拦截     → cooling 2h
//   其它 4xx     → cooling 60s

// modelCoolResetWindow：冷却早已结束、且超过这么久没有再犯，就当作新的一条失败链，
// 从 3 分钟重新开始。否则一个号每天偶尔 429 一次，冷却时间会无限累加。
const modelCoolResetWindow = 24 * time.Hour

// modelCoolMax：单次模型冷却上限。
const modelCoolMax = 24 * time.Hour

type CooldownBook struct {
	st *store.Store

	mu           sync.Mutex
	modelEnabled bool
	models       map[string]modelCool
	hydrated     bool
}

type modelCool struct {
	accID  string // 原始大小写，内存 key 统一小写，展示时要还原
	model  string // 原始大小写，用于面板展示
	until  int64
	hits   int64
	reason string
}

// ModelCoolInfo 是面板上「限流」列的一条明细。
type ModelCoolInfo struct {
	Model  string `json:"model"`
	Until  int64  `json:"until"`
	Reason string `json:"reason"`
	Hits   int64  `json:"hits"`
}

func NewCooldownBook(st *store.Store, modelCooldownEnabled bool) *CooldownBook {
	return &CooldownBook{st: st, modelEnabled: modelCooldownEnabled, models: map[string]modelCool{}}
}

// SetModelCooldown 供面板热更新「429 是否按账号×模型冷却」。
func (c *CooldownBook) SetModelCooldown(on bool) {
	c.mu.Lock()
	c.modelEnabled = on
	c.mu.Unlock()
}

func (c *CooldownBook) ModelEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.modelEnabled
}

func coolKey(accID, model string) string { return strings.ToLower(accID + "∥" + model) }

// Hydrate 从库里恢复模型冷却。进程重启后内存表是空的，不恢复的话
// 正在冷却中的「账号×模型」会被立刻重新选中。
func (c *CooldownBook) Hydrate(ctx context.Context) {
	c.mu.Lock()
	if c.hydrated {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	rows, err := c.st.ListModelCooldowns(ctx)
	if err != nil {
		return // 读失败就下次再试，不要把 hydrated 置位
	}
	c.mu.Lock()
	for _, r := range rows {
		c.models[coolKey(r.AccountID, r.Model)] = modelCool{
			accID: r.AccountID, model: r.Model, until: r.Until, hits: r.Hits, reason: r.Reason,
		}
	}
	c.hydrated = true
	c.mu.Unlock()
}

// ModelCooldownsOf 返回某个账号当前仍在冷却中的「账号×模型」明细。
// 已到期的记录不返回：它们只是失败链的历史，不该再显示成「限流中」。
func (c *CooldownBook) ModelCooldownsOf(accID string) []ModelCoolInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UnixMilli()
	var out []ModelCoolInfo
	for _, m := range c.models {
		if m.until <= now || !strings.EqualFold(m.accID, accID) {
			continue
		}
		out = append(out, ModelCoolInfo{Model: m.model, Until: m.until, Reason: m.reason, Hits: m.hits})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until < out[j].Until })
	return out
}

// ClearModelNow 手动解除一条「账号×模型」冷却，同时清空失败链。
// 面板上的「清除」用它——用户比阶梯算法更清楚那个模型是不是真的还能用。
func (c *CooldownBook) ClearModelNow(ctx context.Context, accID, model string) {
	c.ClearModel(ctx, accID, model)
}

func (c *CooldownBook) IsModelCooled(accID, model string) bool {
	if model == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.modelEnabled {
		return false
	}
	k := coolKey(accID, model)
	m, ok := c.models[k]
	if !ok {
		return false
	}
	// 到期后不能删记录：CoolModel 靠它拿到上一档 hits 才能继续拉长冷却。
	// 在这里删掉的话，到期后第一次再 429 会从 3 分钟重新开始，阶梯永远停在一档。
	// 真正的清除只发生在调用成功（ClearModel）或超过断链窗口时。
	return m.until > time.Now().UnixMilli()
}

// modelCoolDelay 返回一条连续失败链上第 n 次（从 0 开始）命中的冷却时长：
// 3 分钟 → 5 分钟 → 10 分钟，之后逐次翻倍，封顶 24 小时。
func modelCoolDelay(hits int64) time.Duration {
	switch hits {
	case 0:
		return 3 * time.Minute
	case 1:
		return 5 * time.Minute
	case 2:
		return 10 * time.Minute
	}
	if hits > 12 {
		return modelCoolMax
	}
	d := 10 * time.Minute * time.Duration(int64(1)<<(hits-2))
	if d > modelCoolMax {
		d = modelCoolMax
	}
	return d
}

// CoolModel 记一次模型级失败（429 / 模型级限流）。同一账号同一模型连续失败
// 逐次拉长冷却；冷却到期后再失败会继续上一档，不会退回 3 分钟。
// 成功返回时由 ClearModel 清零。
func (c *CooldownBook) CoolModel(ctx context.Context, accID, model, reason string) {
	if model == "" || !c.ModelEnabled() {
		return
	}
	now := time.Now().UnixMilli()
	c.mu.Lock()
	k := coolKey(accID, model)
	prev, ok := c.models[k]
	hits := int64(0)
	if ok {
		// 冷却结束超过 ResetWindow 还没有新的失败：视为失败链已断，从头再来。
		if prev.until > 0 && now-prev.until <= modelCoolResetWindow.Milliseconds() {
			hits = prev.hits
		}
	}
	delay := modelCoolDelay(hits)
	m := modelCool{accID: accID, model: model, until: now + delay.Milliseconds(), hits: hits + 1, reason: reason}
	c.models[k] = m
	// 过期的失败链在这里顺手清掉，免得 map 只增不减。
	// 注意每个 key 都要自己判过期（不能像以前那样依赖 IsModelCooled 删除——
	// 删了就丢了 hits，阶梯永远停在 3 分钟）。
	for key, v := range c.models {
		if v.until+modelCoolResetWindow.Milliseconds() < now {
			delete(c.models, key)
		}
	}
	c.mu.Unlock()
	_ = c.st.UpsertModelCooldown(ctx, store.ModelCooldown{
		AccountID: accID, Model: model, Until: m.until, Reason: reason, Hits: m.hits,
	})
}

// ClearModel 在一次成功调用后解除该「账号×模型」的冷却并清空失败链。
func (c *CooldownBook) ClearModel(ctx context.Context, accID, model string) {
	if model == "" {
		return
	}
	c.mu.Lock()
	k := coolKey(accID, model)
	_, ok := c.models[k]
	delete(c.models, k)
	c.mu.Unlock()
	if ok {
		_ = c.st.DeleteModelCooldown(ctx, accID, model)
	}
}

// FailKind 是失败分类，由转发层判定后交给号池执行冷却。
type FailKind string

const (
	FailQuota       FailKind = "quota"        // 402
	FailRate        FailKind = "rate"         // 429（模型级冷却关闭时的兜底）
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
//
// 两种状态都会到期：
//   - cooling：429/5xx/风控的临时冷却；
//   - exhausted 且 CoolUntil > 0：402 积分耗尽，冷却到次日 04:00。
//
// CoolUntil 为 0 的 exhausted 是「等签到/余额回升」的阈值暂停，交给
// ApplyCreditPolicy 处理，这里不碰，否则会绕过低积分规则把它放出来。
func (c *CooldownBook) ReviveExpired(ctx context.Context) {
	accounts, err := c.st.ListAccounts(ctx)
	if err != nil {
		return
	}
	now := time.Now().UnixMilli()
	for _, a := range accounts {
		switch a.Status {
		case store.StatusCooling, store.StatusExhausted:
			// 只有带到期时刻的冷却才在这里复活；0 表示阈值暂停，跳过。
			if a.CoolUntil == 0 || a.CoolUntil > now {
				continue
			}
		default:
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
