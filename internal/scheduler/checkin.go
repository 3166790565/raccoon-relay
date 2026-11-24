package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/creditsync"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Scheduler 负责每日自动签到。
//
// 设计要点：
//   · 每个账号的计划时刻 = 用户设定的基准时刻 + 按 uid 派生的确定性抖动（同一账号每天固定，重启不变）
//   · granted:true 与 granted:false 都算当日完成，都写 last_checkin_day（后者是"今日已领过"，幂等）
//   · 401 且刷新失败 → 当日不再重试；5xx/网络错误 → 不标记，下一 tick 自然重试
//   · 同账号当日连续失败 3 次 → 当日熔断
//   · 账号间串行，间隔随机，防风控

const (
	autoFailThreshold = 3
	tickInterval      = 60 * time.Second
)

type Scheduler struct {
	cfg      *config.Config
	st       *store.Store
	client   *raccoon.Client
	refresh  *relay.Refresher
	balancer *creditsync.Balancer

	mu         sync.Mutex
	lastTick   time.Time
	lastRun    map[string]string // accountID → 当日已执行的动作标记
	running    bool
	lastSweep  time.Time
	lastResult string
	lastCount  int

	// onEvent 把签到结果写成运行事件，由装配层注入。
	onEvent func(store.RuntimeEvent)

	// locker 用于多实例协调。nil 表示单实例，行为与改造前一致。
	// 锁的粒度是「每个账号」，而不是整轮 sweep：一轮可能持续数分钟，
	// 整轮加锁要么 TTL 太短被别人重入，要么太长导致崩溃后长期不恢复。
	locker Locker
}

// Locker 是分布式锁的最小接口，由 internal/cache 实现。
type Locker interface {
	Lock(ctx context.Context, name string, ttl time.Duration) (release func(), ok bool)
}

// SetLocker 注入分布式锁。传 nil 恢复单实例行为。
func (s *Scheduler) SetLocker(l Locker) {
	s.mu.Lock()
	s.locker = l
	s.mu.Unlock()
}

// tryLockCheckin 尝试锁住某个账号的当日签到。返回的 release 必须在结束后调用。
// 无 locker 时恒成功。
func (s *Scheduler) tryLockCheckin(ctx context.Context, day, accountID string) (func(), bool) {
	s.mu.Lock()
	l := s.locker
	s.mu.Unlock()
	if l == nil {
		return func() {}, true
	}
	return l.Lock(ctx, "checkin:"+day+":"+accountID, 5*time.Minute)
}

// SetEventHook 注入运行事件钩子。
func (s *Scheduler) SetEventHook(fn func(store.RuntimeEvent)) {
	s.mu.Lock()
	s.onEvent = fn
	s.mu.Unlock()
}

func (s *Scheduler) emit(ev store.RuntimeEvent) {
	s.mu.Lock()
	fn := s.onEvent
	s.mu.Unlock()
	if fn == nil {
		return
	}
	if ev.Scope == "" {
		ev.Scope = store.ScopeCheckin
	}
	fn(ev)
}

func New(cfg *config.Config, st *store.Store, client *raccoon.Client, refresh *relay.Refresher,
	balancer *creditsync.Balancer) *Scheduler {
	return &Scheduler{
		cfg: cfg, st: st, client: client, refresh: refresh, balancer: balancer,
		lastRun: map[string]string{},
	}
}

// Run 启动定时循环，阻塞直到 ctx 取消。
//
// 循环常驻，每轮重新读 CheckinEnable：面板里开关自动签到要能立即生效，
// 不能在启动时判断一次就 return（那样之后从面板打开也永远不会跑）。
func (s *Scheduler) Run(ctx context.Context) {
	if !s.enabled() {
		log.Println("[签到] 自动签到已关闭（CHECKIN_ENABLE=false）")
	} else {
		// 启动即补跑一轮
		s.sweep(ctx)
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.enabled() {
				s.sweep(ctx)
			}
		}
	}
}

// enabled 读运行期开关。配置由面板热更新，这里每次现读。
func (s *Scheduler) enabled() bool {
	return s.cfg.CheckinEnable
}

// SetEnabled 面板切换自动签到。
func (s *Scheduler) SetEnabled(on bool) {
	s.mu.Lock()
	s.cfg.CheckinEnable = on
	s.mu.Unlock()
	if on {
		log.Println("[签到] 自动签到已开启")
	} else {
		log.Println("[签到] 自动签到已关闭")
	}
}

// Status 供管理面展示。
func (s *Scheduler) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 从未跑过时 lastTick 是零值，UnixMilli() 会给出 -62135596800000；
	// 面板拿到这个数会显示成日期，所以统一收敛成 0 表示「尚未执行」。
	var lastTick int64
	if !s.lastTick.IsZero() {
		lastTick = s.lastTick.UnixMilli()
	}
	return map[string]any{
		"enabled":     s.cfg.CheckinEnable,
		"running":     s.running,
		"lastTick":    lastTick,
		"lastResult":  s.lastResult,
		"lastCount":   s.lastCount,
		"tickSeconds": int(tickInterval.Seconds()),
	}
}

// hasCheckinCred 判断账号是否有可用于签到的凭据（小浣熊看 Token 列）。
func hasCheckinCred(a *store.Account) bool {
	return a.Token != ""
}

func (s *Scheduler) sweep(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.lastTick = time.Now()
		s.mu.Unlock()
	}()

	loc := s.cfg.Location()
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")

	accounts, err := s.st.ListAccounts(ctx)
	if err != nil {
		log.Printf("[签到] 读取账号失败: %v", err)
		return
	}

	var due []*store.Account
	for _, a := range accounts {
		// 日日新没有「每日签到领积分」这一步（额度按 5 小时/本周窗口滚动），
		// 强行签到只会打无意义的接口。它们的额度靠余额同步拉取。
		if a.IsSenseNova() {
			continue
		}
		if !a.Enabled || !hasCheckinCred(a) {
			continue
		}
		if a.LastCheckinDay == today {
			continue
		}
		if now.Before(plannedTime(a.UID, loc, s.cfg.CheckinHour, s.cfg.CheckinMinute, s.cfg.CheckinJitterMin)) {
			continue
		}
		due = append(due, a)
	}
	if len(due) == 0 {
		s.mu.Lock()
		s.lastResult = "无待签到账号（" + today + "）"
		s.lastCount = 0
		s.mu.Unlock()
		return
	}

	done, failed := 0, 0
	for _, a := range due {
		if ctx.Err() != nil {
			return
		}
		// 每个账号单独加锁：多实例下只有一个实例会签这个号，
		// 其余实例本轮跳过。
		release, locked := s.tryLockCheckin(ctx, today, a.ID)
		if !locked {
			continue
		}
		ok := s.checkinOne(ctx, a, today, loc, store.CheckinSourceAuto)
		release()
		if ok {
			done++
		} else {
			failed++
		}
		// 账号间随机间隔，防风控
		gap := s.cfg.CheckinGapMinMS
		if s.cfg.CheckinGapMaxMS > gap {
			gap += rand.Intn(s.cfg.CheckinGapMaxMS - gap + 1)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(gap) * time.Millisecond):
		}
	}
	s.mu.Lock()
	s.lastResult = fmt.Sprintf("签到完成：成功 %d，失败 %d", done, failed)
	s.lastCount = len(due)
	s.mu.Unlock()
	log.Printf("[签到] %s", s.lastResult)
}

// checkinOne 执行单账号签到链。返回是否当日完成。
// source 只用于记录触发来源（定时/手动/新号），不参与任何判断。
func (s *Scheduler) checkinOne(ctx context.Context, a *store.Account, today string, loc *time.Location, source string) bool {
	// 连续失败太多则熔断
	if n, err := s.st.CheckinFailCount(ctx, a.ID, today, "checkin"); err == nil && n >= autoFailThreshold {
		return false
	}

	token, err := s.refresh.EnsureFresh(ctx, a)
	if err != nil {
		s.fail(ctx, a, today, err.Error(), source)
		return false
	}

	idn := raccoon.IdentityFor(a.UID, a.DeviceID, a.OSVersion, a.Platform, a.ClientChannel, a.OfficeIdentity, a.Language)
	res, err := s.client.Checkin(ctx, token, idn)
	if err != nil && isCheckinDeadToken(err) {
		// 同对话面：EnsureFresh 只在临期窗口内刷新，手里这枚 access 可能已被上游判过期。
		// 先强刷一次，刷得动就继续签到；刷不动才算这个号真要重登。
		if fresh, rerr := s.refresh.RefreshNow(ctx, a); rerr == nil {
			token = fresh
			res, err = s.client.Checkin(ctx, token, idn)
		} else if isCheckinDeadToken(rerr) {
			// refresh_token 真的失效了，当日不再重试
			relogin := store.StatusRelogin
			_ = s.st.UpdateAccount(ctx, a.ID, store.AccountPatch{
				Status: &relogin, CoolReason: strPtr("凭证失效，需重新登录"),
			})
			s.fail(ctx, a, today, "凭证失效："+rerr.Error(), source)
			return false
		} else {
			// 刷新请求本身失败（网络/上游抖动）：保留原错误，下一轮自然重试
			s.fail(ctx, a, today, err.Error(), source)
			return false
		}
	}
	if err != nil {
		// 强刷并重签之后仍然被鉴权挡下，才认定这个号要重登
		if isCheckinDeadToken(err) {
			relogin := store.StatusRelogin
			_ = s.st.UpdateAccount(ctx, a.ID, store.AccountPatch{
				Status: &relogin, CoolReason: strPtr("凭证失效，需重新登录"),
			})
			s.fail(ctx, a, today, "凭证失效："+err.Error(), source)
			return false
		}
		s.fail(ctx, a, today, err.Error(), source)
		return false
	}
	if !res.OK {
		s.fail(ctx, a, today, res.Message, source)
		return false
	}

	// 首登奖：只发未结算的键
	ledger := readLedger(a.Meta)
	settled, msgs, _ := s.client.ClaimOnboarding(ctx, token, idn, ledger)
	if len(settled) > 0 {
		mergeLedger(a.Meta, settled)
		_ = s.st.UpdateAccount(ctx, a.ID, store.AccountPatch{Meta: a.Meta})
		for _, m := range msgs {
			log.Printf("[签到] %s 首登奖 %s", a.Name, m)
			s.emit(store.RuntimeEvent{Level: store.LevelInfo, Component: "checkin",
				AccountID: a.ID, Message: "首登奖到账", Detail: a.Name + "：" + m})
		}
	}

	day := today
	online := store.StatusOnline
	zero := int64(0)
	empty := ""
	_ = s.st.UpdateAccount(ctx, a.ID, store.AccountPatch{
		LastCheckinDay: &day, Status: &online, CoolUntil: &zero, CoolReason: &empty,
	})
	// 积分用账单核对出的**真实入账**，不再是写死的 0：
	// 发放接口回 200 不代表今天真的发了，账单里看得到才算数。
	_ = s.st.AddCheckin(ctx, store.CheckinRecord{
		AccountID: a.ID, Day: today, Action: "checkin", Source: source, OK: true,
		Already: res.Already, Credit: res.Credit, Message: res.Message,
	})

	// 顺带刷一次余额
	s.refreshCredits(ctx, a)

	log.Printf("[签到] %s：%s", a.Name, res.Message)
	s.emit(store.RuntimeEvent{Level: store.LevelInfo, Component: "checkin",
		AccountID: a.ID, Message: "签到成功", Detail: a.Name + "：" + res.Message})
	_ = loc
	return true
}

func (s *Scheduler) refreshCredits(ctx context.Context, a *store.Account) {
	// 失败静默：余额是辅助信息，不该让签到流程因为查不到余额而失败
	_, _ = s.balancer.Refresh(ctx, a)
}

func (s *Scheduler) fail(ctx context.Context, a *store.Account, today, msg, source string) {
	_ = s.st.AddCheckin(ctx, store.CheckinRecord{
		AccountID: a.ID, Day: today, Action: "checkin", Source: source, OK: false, Message: msg,
	})
	log.Printf("[签到] %s 失败：%s", a.Name, msg)
	s.emit(store.RuntimeEvent{Level: store.LevelWarn, Component: "checkin",
		AccountID: a.ID, Message: "签到失败", Detail: a.Name + "：" + msg})
}

// plannedTime 计算账号今日的计划签到时刻：基准 + uid 派生的确定性抖动。
func plannedTime(uid string, loc *time.Location, hour, minute, jitterMin int) time.Time {
	now := time.Now().In(loc)
	base := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)
	if jitterMin <= 0 {
		return base
	}
	h := sha256.Sum256([]byte("checkin-jitter:" + uid))
	off := int(binary.BigEndian.Uint32(h[:4]) % uint32(jitterMin+1))
	return base.Add(time.Duration(off) * time.Minute)
}

// NextPlannedCheckin 返回该账号下一次自动签到的时刻（今日未到就是今日，已过算明日）。
//
// 供号池的 402/1000007 冷却使用：积分耗尽的号要停在「下一次签到补分」之前，
// 而不是写死一个和签到时间无关的次日 04:00。自动签到关闭时返回 false，
// 调用方自行回退到默认恢复时刻。
func (s *Scheduler) NextPlannedCheckin(a *store.Account) (time.Time, bool) {
	if a == nil || !s.enabled() {
		return time.Time{}, false
	}
	loc := s.cfg.Location()
	now := time.Now().In(loc)
	t := plannedTime(a.UID, loc, s.cfg.CheckinHour, s.cfg.CheckinMinute, s.cfg.CheckinJitterMin)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t, true
}

func readLedger(meta map[string]any) map[string]int64 {
	out := map[string]int64{}
	raw, ok := meta["onboardingGrants"].(map[string]any)
	if !ok {
		return out
	}
	for k, v := range raw {
		switch t := v.(type) {
		case float64:
			out[k] = int64(t)
		case int64:
			out[k] = t
		case int:
			out[k] = int64(t)
		}
	}
	return out
}

func mergeLedger(meta map[string]any, settled map[string]int64) {
	cur, _ := meta["onboardingGrants"].(map[string]any)
	if cur == nil {
		cur = map[string]any{}
	}
	for k, v := range settled {
		cur[k] = v
	}
	meta["onboardingGrants"] = cur
}

// Onboard 是账号刚加入时的立刻初始化：签到一次 + 同步余额。
//
// 为什么不做成异步后台任务：用户刚在面板上点完「兑换并添加」，最想看到的就是
// 这个号有多少积分、今天领没领。异步跑完再去拉列表，中间会有一段时间面板显示
// 「积分 -」，反而让人以为导入失败了。这里同步跑完再返回，代价是一次上游往返。
//
// 失败不阻断入库：凭据已经存好了，签到失败（比如刚好被风控）不影响它以后被选中。
func (s *Scheduler) Onboard(ctx context.Context, accountID string) (map[string]any, error) {
	acc, err := s.st.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, fmt.Errorf("账号不存在: %s", accountID)
	}
	loc := s.cfg.Location()
	today := time.Now().In(loc).Format("2006-01-02")

	// 日日新没有签到：新号落地只同步额度；同时尽量补建一枚推理 Key，
	// 这样第一次请求就不必在转发路径上等一次建 Key。
	if acc.IsSenseNova() {
		if c, err := s.balancer.Refresh(ctx, acc); err == nil {
			acc.Credits = c
		}
		return map[string]any{"checkinDay": "", "credits": acc.Credits}, nil
	}

	// 签到（内部会顺带刷余额）
	s.checkinOne(ctx, acc, today, loc, store.CheckinSourceOnboard)

	// 再单独刷一次余额兜底：签到可能因为「今日已领」跳过部分流程，
	// 而新号最需要的是把余额拉下来。
	if c, err := s.balancer.Refresh(ctx, acc); err == nil {
		acc.Credits = c
	}
	return map[string]any{
		"checkinDay": acc.LastCheckinDay,
		"credits":    acc.Credits,
	}, nil
}

// RunAll 供管理面「全部签到」手动触发：不检查计划时刻，逐号串行。
func (s *Scheduler) RunAll(ctx context.Context) map[string]any {
	loc := s.cfg.Location()
	today := time.Now().In(loc).Format("2006-01-02")
	accounts, err := s.st.ListAccounts(ctx)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	var results []map[string]any
	done := 0
	for _, a := range accounts {
		if !a.Enabled {
			results = append(results, map[string]any{"id": a.ID, "name": a.Name, "ok": false, "message": "账号已禁用"})
			continue
		}
		if a.IsSenseNova() {
			// 日日新没有签到；只刷新额度，结果照常回填一行，用户能看到它在跑。
			_, _ = s.balancer.Refresh(ctx, a)
			results = append(results, map[string]any{
				"id": a.ID, "name": a.Name, "ok": true, "message": "日日新账号：已同步额度（无签到）",
			})
			done++
			continue
		}
		ok := s.checkinOne(ctx, a, today, loc, store.CheckinSourceManual)
		if ok {
			done++
		}
		results = append(results, map[string]any{"id": a.ID, "name": a.Name, "ok": ok})
	}
	return map[string]any{"ok": true, "done": done, "total": len(accounts), "results": results}
}

func strPtr(s string) *string { return &s }

// isAuthErr 判断错误是否属于「这枚 access 不认」。只认显式 Auth 标记：
// 403 不带标记时交给对话面的风控分类，签到这里不据此打 relogin。
func isAuthErr(err error) bool {
	var ue *raccoon.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	return ue.Auth
}

// isCheckinDeadToken 判断签到用到的 access 是否真的不可用（而不是签到端点临时抽风）。
// 只认 Auth 标记或 401；这类才值得强刷后重签。
func isCheckinDeadToken(err error) bool {
	var ue *raccoon.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	return ue.Auth || ue.Status == http.StatusUnauthorized
}
