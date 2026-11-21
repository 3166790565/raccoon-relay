package pool

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 冷却分级（只有这条列表里的失败会停号）：
//   402 积分耗尽 → exhausted 至次日 04:00
//   429 限流     → 默认只罚「账号 × 模型」，阶梯 3 → 5 → 10 → 20… 分钟；
//                  关闭模型级冷却时才退化为整号 60s
//   200108 账号异常 → 账号级阶梯冷却，同样 3 → 5 → 10… 分钟；换模型绕不过去
//   401 刷新失败 → relogin 永久
//
// 不冷却、只换号重试的失败（NonCooling）：
//   403 / 5xx / 网络 → 上游波动，换号即可，账号保持在线
//   100041/100042 敏感词 → 请求内容被上游拒答，与账号无关，不换号直接返回

// coolResetWindow：冷却早已结束、且超过这么久没有再犯，就当作新的一条失败链，
// 从 3 分钟重新开始。否则一个号每天偶尔 429 一次，冷却时间会无限累加。
const coolResetWindow = 24 * time.Hour

// coolMax：单次冷却上限。
const coolMax = 24 * time.Hour

type CooldownBook struct {
	st *store.Store

	mu            sync.Mutex
	modelEnabled  bool
	accountAbnorm bool
	models        map[string]modelCool
	accounts      map[string]accountCool
	hydrated      bool

	// onEvent 由上层（admin/main）注入，用来把冷却动作写进运行日志。
	// 用回调而不是直接依赖 store 的写入封装，是为了让号池保持无副作用、易测试；
	// 事件写入失败也不会影响冷却本身（回调内部吞错）。
	onEvent func(Event)
}

// Event 是从号池抛给运行日志的一条事件。
type Event struct {
	Level     string // info / warn / error
	Scope     string
	Component string
	AccountID string
	Message   string
	Detail    string
}

// SetEventHook 注入事件钩子。传 nil 可关闭。
func (c *CooldownBook) SetEventHook(fn func(Event)) {
	c.mu.Lock()
	c.onEvent = fn
	c.mu.Unlock()
}

func (c *CooldownBook) emit(ev Event) {
	c.mu.Lock()
	fn := c.onEvent
	c.mu.Unlock()
	if fn == nil {
		return
	}
	if ev.Scope == "" {
		ev.Scope = store.ScopePool
	}
	fn(ev)
}

type modelCool struct {
	accID  string // 原始大小写，内存 key 统一小写，展示时要还原
	model  string // 原始大小写，用于面板展示
	until  int64
	hits   int64
	reason string
}

// accountCool 是「账号异常」类上游处罚的失败链，按 hits 阶梯拉长。
type accountCool struct {
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
	return &CooldownBook{
		st: st, modelEnabled: modelCooldownEnabled, accountAbnorm: true,
		models: map[string]modelCool{}, accounts: map[string]accountCool{},
	}
}

// NewCooldownBookWithOptions 是带完整默认值的构造入口：装配层按环境变量
// 传入 200108 账号级冷却的初始开关，其余调用方仍可用 NewCooldownBook。
func NewCooldownBookWithOptions(st *store.Store, modelCooldownEnabled, accountAbnormalCool bool) *CooldownBook {
	c := NewCooldownBook(st, modelCooldownEnabled)
	c.accountAbnorm = accountAbnormalCool
	return c
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

// SetAccountAbnormalCool 供面板热更新「200108 账号异常是否按账号冷却」。
// 关闭后 200108 按普通上游失败处理（10 分钟冷却并换号），适合只在调试时用。
func (c *CooldownBook) SetAccountAbnormalCool(on bool) {
	c.mu.Lock()
	c.accountAbnorm = on
	c.mu.Unlock()
}

func (c *CooldownBook) AccountAbnormalEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accountAbnorm
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
	accRows, err := c.st.ListAccountCooldowns(ctx)
	if err != nil {
		return
	}
	c.mu.Lock()
	for _, r := range rows {
		c.models[coolKey(r.AccountID, r.Model)] = modelCool{
			accID: r.AccountID, model: r.Model, until: r.Until, hits: r.Hits, reason: r.Reason,
		}
	}
	for _, r := range accRows {
		c.accounts[r.AccountID] = accountCool{until: r.Until, hits: r.Hits, reason: r.Reason}
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

// ClearAccountNow 手动解除账号级冷却：内存失败链、落库记录与账号状态一起清掉。
// 用户点「解除限流」就是认为这个号现在能跑，留着 hits 只会让下次失败直接跳到高档。
func (c *CooldownBook) ClearAccountNow(ctx context.Context, accID string) {
	c.ClearAccountLadder(ctx, accID)
	c.MarkOnline(ctx, accID)
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

// coolDelay 返回一条连续失败链上第 n 次（从 0 开始）命中的冷却时长：
// 3 分钟 → 5 分钟 → 10 分钟，之后逐次翻倍，封顶 24 小时。
// 模型级与账号级冷却共用这条阶梯，面板上的说明也按它写。
func coolDelay(hits int64) time.Duration {
	switch hits {
	case 0:
		return 3 * time.Minute
	case 1:
		return 5 * time.Minute
	case 2:
		return 10 * time.Minute
	}
	if hits > 12 {
		return coolMax
	}
	d := 10 * time.Minute * time.Duration(int64(1)<<(hits-2))
	if d > coolMax {
		d = coolMax
	}
	return d
}

// modelCoolDelay 保留旧名，模型级冷却与测试都用它。
func modelCoolDelay(hits int64) time.Duration { return coolDelay(hits) }

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
		if prev.until > 0 && now-prev.until <= coolResetWindow.Milliseconds() {
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
		if v.until+coolResetWindow.Milliseconds() < now {
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

// IsAccountLadderCooled 判断账号是否还在「账号异常」阶梯冷却中。
// 这条冷却写在 accounts.status/cool_until 上，这里只维护失败链的命中次数，
// 两者由 CoolAccountLadder 一起写。
func (c *CooldownBook) IsAccountLadderCooled(accID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.accounts[accID]
	return ok && a.until > time.Now().UnixMilli()
}

// AccountCooldownOf 返回账号级失败链的当前状态，供面板展示命中次数。
func (c *CooldownBook) AccountCooldownOf(accID string) (until, hits int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, found := c.accounts[accID]
	return a.until, a.hits, found
}

// AccountCoolInfo 是面板上「账号级冷却」的明细：当前命中第几档、还剩多久。
type AccountCoolInfo struct {
	Until  int64  `json:"until"`
	Hits   int64  `json:"hits"`
	Reason string `json:"reason"`
}

// AccountCoolOf 返回仍生效的账号级冷却明细；已到期或没有记录时返回 nil。
func (c *CooldownBook) AccountCoolOf(accID string) *AccountCoolInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.accounts[accID]
	if !ok || a.until <= time.Now().UnixMilli() {
		return nil
	}
	return &AccountCoolInfo{Until: a.until, Hits: a.hits, Reason: a.reason}
}

// CoolAccountLadder 记一次「整个账号被上游处罚」（200108 账号异常等）。
//
// 和 429 的模型级冷却用的是同一条阶梯（3 → 5 → 10 → 20… 分钟，封顶 24 小时），
// 区别只在处罚范围：这次是账号级，状态写成 cooling，选号时整号跳过。
// 上游文案通常明确写了「请稍后重试」，所以换号继续消化是对的；
// 但不能把账号永久停掉——阶梯到期由 ReviveExpired 自动放回在线。
func (c *CooldownBook) CoolAccountLadder(ctx context.Context, accID, reason string) {
	now := time.Now().UnixMilli()
	c.mu.Lock()
	prev, ok := c.accounts[accID]
	hits := int64(0)
	if ok && prev.until > 0 && now-prev.until <= coolResetWindow.Milliseconds() {
		hits = prev.hits
	}
	delay := coolDelay(hits)
	a := accountCool{until: now + delay.Milliseconds(), hits: hits + 1, reason: reason}
	c.accounts[accID] = a
	for key, v := range c.accounts {
		if v.until+coolResetWindow.Milliseconds() < now {
			delete(c.accounts, key)
		}
	}
	c.mu.Unlock()

	status := store.StatusCooling
	_ = c.st.UpdateAccount(ctx, accID, store.AccountPatch{
		Status: &status, CoolUntil: &a.until, CoolReason: &reason,
	})
	_ = c.st.UpsertAccountCooldown(ctx, store.AccountCooldown{
		AccountID: accID, Until: a.until, Reason: reason, Hits: a.hits,
	})
	c.emitWhy(accID, store.LevelWarn, "账号级冷却：上游判定账号异常", reason, a.until)
}

// ClearAccountLadder 在账号重新调用成功后清空失败链。
//
// 只清内存与库里的命中次数，不碰账号状态：账号可能因为 402/风控还处在别的
// 冷却里，这里顺手改状态会把那层冷却绕过去。状态由 MarkOnline/ReviveExpired 管。
func (c *CooldownBook) ClearAccountLadder(ctx context.Context, accID string) {
	c.mu.Lock()
	_, ok := c.accounts[accID]
	delete(c.accounts, accID)
	c.mu.Unlock()
	if ok {
		_ = c.st.DeleteAccountCooldown(ctx, accID)
	}
}

// FailKind 是失败分类，由转发层判定后交给号池执行冷却。
type FailKind string

const (
	FailQuota           FailKind = "quota"            // 402
	FailRate            FailKind = "rate"             // 429（模型级冷却关闭时的兜底）
	FailTransient       FailKind = "transient"        // 403 / 5xx / 网络：上游波动，不冷却，直接换号
	FailRelogin         FailKind = "relogin"          // 401 且刷新失败
	FailRejected        FailKind = "rejected"         // 100041/100042 敏感词拒答：与账号无关，不冷却、不换号
	FailModelScoped     FailKind = "model_scoped"     // 模型级问题，罚账号×模型
	FailAccountAbnormal FailKind = "account_abnormal" // 200108 账号异常，账号级阶梯冷却
	FailOther           FailKind = "other"
)

// Cool 按分类写账号冷却。
//
// 只有 402（积分耗尽）、401（凭证失效）与 429 兜底会真正停号；
// 403 / 5xx / 网络这类上游波动不在这里停号（见 NonCooling），
// 否则一次偶发 5xx 就白罚一个号 10 分钟。
func (c *CooldownBook) Cool(ctx context.Context, accID string, kind FailKind, detail string) {
	now := time.Now().UnixMilli()
	var until int64
	var status, msg, level string
	switch kind {
	case FailQuota:
		until = nextDay4AM()
		status, msg, level = store.StatusExhausted, defaultStr(detail, "积分耗尽"), store.LevelWarn
	case FailRate:
		until = now + 60_000
		status, msg, level = store.StatusCooling, defaultStr(detail, "上游限流"), store.LevelWarn
	case FailAccountAbnormal:
		// 账号异常走阶梯：这里只是兜底，正常路径由 CoolAccountLadder 处理（它带命中次数）。
		until = now + coolDelay(0).Milliseconds()
		status, msg, level = store.StatusCooling, defaultStr(detail, "上游判定账号异常"), store.LevelWarn
	case FailRelogin:
		status, msg, level = store.StatusRelogin, defaultStr(detail, "凭证失效，需重新登录"), store.LevelError
	case FailTransient, FailRejected, FailModelScoped:
		// 上游波动（403/5xx/网络）与内容拒答不停号：前者换号即可，
		// 后者换号也没用。这里什么都不写，保持账号在线。
		return
	default:
		until = now + 60_000
		status, msg, level = store.StatusCooling, detail, store.LevelWarn
	}
	c.setAccount(ctx, accID, status, until, msg)
	c.emitWhy(accID, level, "停用账号 "+statusLabel(status), msg, until)
}

// NonCooling 判断这类失败是否不冷却账号、只换号重试。
// 转发层用它决定「不写冷却事件」时，不会把未停号的账号误报成已冷却。
func NonCooling(kind FailKind) bool {
	switch kind {
	case FailTransient, FailRejected:
		return true
	}
	return false
}

// CoolUntil 按绝对时刻冷却，用于对齐 Retry-After。
func (c *CooldownBook) CoolUntil(ctx context.Context, accID string, until int64, detail string) {
	if until <= time.Now().UnixMilli() {
		until = time.Now().UnixMilli() + 1000
	}
	msg := defaultStr(detail, "上游限流")
	c.setAccount(ctx, accID, store.StatusCooling, until, msg)
	c.emitWhy(accID, store.LevelWarn, "按 Retry-After 冷却", msg, until)
}

// emitWhy 把一次冷却写成事件，detail 里带原因与恢复时刻，排查时不用再去号池页倒推。
func (c *CooldownBook) emitWhy(accID, level, title, reason string, until int64) {
	detail := reason
	if until > 0 {
		detail = fmt.Sprintf("%s\n恢复时间：%s", reason, time.UnixMilli(until).Format("2006-01-02 15:04:05"))
	}
	c.emit(Event{Level: level, Component: "cooldown", AccountID: accID, Message: title, Detail: detail})
}

func statusLabel(status string) string {
	switch status {
	case store.StatusExhausted:
		return "（积分耗尽）"
	case store.StatusRelogin:
		return "（需重登）"
	case store.StatusCooling:
		return "（冷却）"
	default:
		return ""
	}
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
	// 成功调用说明账号已经恢复：整号的失败链也归零，下次再异常从 3 分钟重新计。
	c.ClearAccountLadder(ctx, accID)
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
		c.emit(Event{Level: store.LevelInfo, Component: "cooldown", AccountID: a.ID,
			Message: "冷却到期，恢复在线", Detail: "原状态：" + a.Status + "；原原因：" + a.CoolReason})
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
