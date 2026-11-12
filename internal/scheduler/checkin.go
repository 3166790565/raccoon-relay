package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/config"
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
	cfg     *config.Config
	st      *store.Store
	client  *raccoon.Client
	refresh *relay.Refresher

	mu        sync.Mutex
	lastTick  time.Time
	lastRun   map[string]string // accountID → 当日已执行的动作标记
	running   bool
	lastSweep time.Time
	lastResult string
	lastCount int
}

func New(cfg *config.Config, st *store.Store, client *raccoon.Client, refresh *relay.Refresher) *Scheduler {
	return &Scheduler{
		cfg: cfg, st: st, client: client, refresh: refresh,
		lastRun: map[string]string{},
	}
}

// Run 启动定时循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	if !s.cfg.CheckinEnable {
		log.Println("[签到] 自动签到已关闭（CHECKIN_ENABLE=false）")
		return
	}
	// 启动即补跑一轮
	s.sweep(ctx)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// Status 供管理面展示。
func (s *Scheduler) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"enabled":     s.cfg.CheckinEnable,
		"lastTick":    s.lastTick.UnixMilli(),
		"lastResult":  s.lastResult,
		"lastCount":   s.lastCount,
		"tickSeconds": int(tickInterval.Seconds()),
	}
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
		if !a.Enabled || a.Token == "" {
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
		ok := s.checkinOne(ctx, a, today, loc)
		if ok {
			done++
		} else {
			failed++
		}
		// 账号间随机间隔，防风控
		gap := s.cfg.CheckinGapMinMS
		if s.cfg.CheckinGapMaxMS > gap {
			gap += rand.Intn(s.cfg.CheckinGapMaxMS-gap+1)
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
func (s *Scheduler) checkinOne(ctx context.Context, a *store.Account, today string, loc *time.Location) bool {
	// 连续失败太多则熔断
	if n, err := s.st.CheckinFailCount(ctx, a.ID, today, "checkin"); err == nil && n >= autoFailThreshold {
		return false
	}

	token, err := s.refresh.EnsureFresh(ctx, a)
	if err != nil {
		s.fail(ctx, a, today, err.Error())
		return false
	}

	idn := raccoon.IdentityFor(a.UID, a.DeviceID, a.OSVersion, a.Platform, a.ClientChannel, a.OfficeIdentity, a.Language)
	res, err := s.client.Checkin(ctx, token, idn)
	if err != nil {
		if ue, ok := err.(*raccoon.UpstreamError); ok && ue.Auth {
			// 凭证失效，当日不再重试
			relogin := store.StatusRelogin
			_ = s.st.UpdateAccount(ctx, a.ID, store.AccountPatch{
				Status: &relogin, CoolReason: strPtr("凭证失效，需重新登录"),
			})
			s.fail(ctx, a, today, "凭证失效："+err.Error())
			return false
		}
		s.fail(ctx, a, today, err.Error())
		return false
	}
	if !res.OK {
		s.fail(ctx, a, today, res.Message)
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
		}
	}

	day := today
	online := store.StatusOnline
	zero := int64(0)
	empty := ""
	_ = s.st.UpdateAccount(ctx, a.ID, store.AccountPatch{
		LastCheckinDay: &day, Status: &online, CoolUntil: &zero, CoolReason: &empty,
	})
	credit := int64(0)
	_ = s.st.AddCheckin(ctx, store.CheckinRecord{
		AccountID: a.ID, Day: today, Action: "checkin", OK: true,
		Already: res.Already, Credit: credit, Message: res.Message,
	})

	// 顺带刷一次余额
	s.refreshCredits(ctx, a, token, idn)

	log.Printf("[签到] %s：%s", a.Name, res.Message)
	_ = loc
	return true
}

func (s *Scheduler) refreshCredits(ctx context.Context, a *store.Account, token string, idn raccoon.Identity) {
	credits, err := s.client.Balance(ctx, token, idn)
	if err != nil {
		return
	}
	at := time.Now().UnixMilli()
	_ = s.st.UpdateAccount(ctx, a.ID, store.AccountPatch{Credits: &credits, CreditsAt: &at})
}

func (s *Scheduler) fail(ctx context.Context, a *store.Account, today, msg string) {
	_ = s.st.AddCheckin(ctx, store.CheckinRecord{
		AccountID: a.ID, Day: today, Action: "checkin", OK: false, Message: msg,
	})
	log.Printf("[签到] %s 失败：%s", a.Name, msg)
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
		ok := s.checkinOne(ctx, a, today, loc)
		if ok {
			done++
		}
		results = append(results, map[string]any{"id": a.ID, "name": a.Name, "ok": ok})
	}
	return map[string]any{"ok": true, "done": done, "total": len(accounts), "results": results}
}

func strPtr(s string) *string { return &s }
