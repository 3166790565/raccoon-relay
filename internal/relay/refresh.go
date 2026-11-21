package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Refresher 负责按账号单飞刷新 token。
//
// 小浣熊的 access_token 是 30 天有效期的 JWT（实测 iat→exp 正好 30 天），
// refresh_token 与它是同一枚凭据。转发路径惰性刷新：只在临期窗口内才刷，
// 抢在客户端之前刷会把凭据轮换掉，反而两边都掉登录。防冷门号静默过期靠
// RenewDue 的主动续期。
type Refresher struct {
	ST     *store.Store
	Client *raccoon.Client
	Window time.Duration

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	locker Locker
}

func NewRefresher(st *store.Store, c *raccoon.Client, window time.Duration) *Refresher {
	if window <= 0 {
		window = 5 * time.Minute
	}
	return &Refresher{ST: st, Client: c, Window: window, locks: map[string]*sync.Mutex{}}
}

// errRefreshBusy 表示另一个实例正在刷新同一账号，本轮先跳过。
var errRefreshBusy = errors.New("该账号正在被其它实例刷新")

func (r *Refresher) lockFor(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.locks[id]
	if !ok {
		l = &sync.Mutex{}
		r.locks[id] = l
	}
	return l
}

// SetLocker 注入跨实例的账号级锁。转发 401 重试、签到、面板刷新都会走
// RefreshNow/EnsureFresh，多副本下必须串行化，否则 refresh_token 连续轮换。
func (r *Refresher) SetLocker(l Locker) {
	r.mu.Lock()
	r.locker = l
	r.mu.Unlock()
}

// lockAccount 先拿进程内单飞锁，再尝试跨实例锁。
// 返回的 release 必须在用完账号后调用；ok=false 表示别的实例正在刷新。
func (r *Refresher) lockAccount(ctx context.Context, id string) (release func(), ok bool) {
	r.mu.Lock()
	l := r.locker
	r.mu.Unlock()
	if l == nil {
		return func() {}, true
	}
	// TTL 必须大于上游超时（默认 120s），否则上游卡住时锁先过期等于没锁。
	return l.Lock(ctx, "refresh:"+id, 180*time.Second)
}

// EnsureFresh 在 access 临期时刷新，返回可用的 token。刷新被拒则返回错误。
func (r *Refresher) EnsureFresh(ctx context.Context, acc *store.Account) (string, error) {
	if !needsRefresh(acc, r.Window) {
		return acc.Token, nil
	}
	l := r.lockFor(acc.ID)
	l.Lock()
	defer l.Unlock()

	release, ok := r.lockAccount(ctx, acc.ID)
	if !ok {
		// 隔壁副本正在刷：直接读库拿它写回的新凭据，读不到就沿用手上的。
		if fresh, err := r.ST.GetAccount(ctx, acc.ID); err == nil && fresh != nil && !needsRefresh(fresh, r.Window) {
			acc.Token, acc.RefreshToken, acc.TokenExp = fresh.Token, fresh.RefreshToken, fresh.TokenExp
			return fresh.Token, nil
		}
		return acc.Token, nil
	}
	defer release()

	// 双检：等锁期间可能已被别的请求刷过
	fresh, err := r.ST.GetAccount(ctx, acc.ID)
	if err == nil && fresh != nil && !needsRefresh(fresh, r.Window) {
		acc.Token, acc.RefreshToken, acc.TokenExp = fresh.Token, fresh.RefreshToken, fresh.TokenExp
		return fresh.Token, nil
	}
	return r.refreshLocked(ctx, acc)
}

// RefreshNow 无视过期时间强制刷新。
//
// 小浣熊的 refresh_token 与 access_token 是同一枚凭据（登录/agent2api 导入/桌面导出
// 三种来源实测都一样）。从 agent2api 导入的号外层 refreshToken 字段是空的，只有
// pluginSecret.accessToken——这里回退用当前 token 续期，否则这类号到期只能重登。
//
// 按账号单飞：续期循环、签到、转发 401 重试、面板「刷新」都会走到这里，
// 并发刷新会让 refresh_token 轮换互相覆盖，后写库的旧值把号打成 relogin。
func (r *Refresher) RefreshNow(ctx context.Context, acc *store.Account) (string, error) {
	l := r.lockFor(acc.ID)
	l.Lock()
	defer l.Unlock()

	// 跨实例锁：隔壁副本可能正在刷同一个号。拿不到就先读库，
	// 若它已经刷完并写回新凭据，直接用新的即可。
	release, ok := r.lockAccount(ctx, acc.ID)
	if !ok {
		if fresh, err := r.ST.GetAccount(ctx, acc.ID); err == nil && fresh != nil &&
			fresh.Token != "" && fresh.Token != acc.Token {
			acc.Token, acc.RefreshToken, acc.TokenExp = fresh.Token, fresh.RefreshToken, fresh.TokenExp
			return fresh.Token, nil
		}
		return "", errRefreshBusy
	}
	defer release()
	return r.refreshLocked(ctx, acc)
}

// refreshLocked 是真正执行刷新的主体，调用方必须已持有该账号的单飞锁。
//
// 上游的 refresh_token 是一次性轮换的：并发刷新时后到的请求会拿到
// 200822「已被并发刷新替换」。拿到这个码时，说明别人刚刷成功并把新凭据写进了库，
// 重新读库用最新凭据再刷一次即可；把这种情况当成「凭证失效」会把好号打成 relogin。
func (r *Refresher) refreshLocked(ctx context.Context, acc *store.Account) (string, error) {
	refreshToken := strings.TrimSpace(acc.RefreshToken)
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(acc.Token)
	}
	if refreshToken == "" {
		return "", &raccoon.UpstreamError{Status: 401, Auth: true, Message: "无 refreshToken 也无 accessToken，请重新导入凭据"}
	}
	idn := identityOf(acc)
	res, err := r.Client.Refresh(ctx, idn, refreshToken)
	if err != nil && isRefreshConflict(err) {
		// 用库里最新的凭据重试一次（可能已被并发的刷新轮换掉了）。
		if fresh, gerr := r.ST.GetAccount(ctx, acc.ID); gerr == nil && fresh != nil {
			latest := strings.TrimSpace(fresh.RefreshToken)
			if latest == "" {
				latest = strings.TrimSpace(fresh.Token)
			}
			if latest != "" && latest != refreshToken {
				res, err = r.Client.Refresh(ctx, identityOf(fresh), latest)
				refreshToken = latest
			}
		}
	}
	if err != nil {
		return "", err
	}
	exp := raccoon.TokenExpiryMillis(res.Token)
	patch := store.AccountPatch{
		Token:        &res.Token,
		RefreshToken: &res.RefreshToken,
		TokenExp:     &exp,
	}
	// 刷新成功说明这枚凭据真的还能用：把被误判成 relogin 的号捞回来。
	// 只清 relogin，不动 cooling/exhausted 的冷却时刻，避免续期顺手绕过风控冷却。
	if acc.Status == store.StatusRelogin {
		online := store.StatusOnline
		zero := int64(0)
		empty := ""
		patch.Status = &online
		patch.CoolUntil = &zero
		patch.CoolReason = &empty
		acc.Status = online
		acc.CoolUntil = 0
		acc.CoolReason = ""
	}
	if oi := strings.TrimSpace(res.OfficeID); oi != "" && oi != acc.OfficeIdentity {
		patch.OfficeIdentity = &oi
		acc.OfficeIdentity = oi
	}
	if err := r.ST.UpdateAccount(ctx, acc.ID, patch); err != nil {
		return "", err
	}
	acc.Token, acc.RefreshToken, acc.TokenExp = res.Token, res.RefreshToken, exp
	return res.Token, nil
}

// RenewResult 是一轮主动续期的统计。
type RenewResult struct {
	Checked int `json:"checked"`
	Renewed int `json:"renewed"`
	Failed  int `json:"failed"`
	// Recovered 是被续期顺手从 relogin 捞回 online 的号。
	Recovered int `json:"recovered"`
	// At 是本轮结束时间（毫秒），供面板显示「上次续期」。
	At int64 `json:"at"`
}

// Renewer 是主动续期的运行期配置持有者。面板改设置时通过 Update 热更新，
// 循环下一轮就会按新参数执行，不需要重启。
//
// 和 logmgr 一样把「配置」与「循环」分开：循环只读快照，避免设置写入时
// 和正在跑的续期互相阻塞。
type Renewer struct {
	ref *Refresher

	mu      sync.Mutex
	cfg     RenewConfig
	wake    chan struct{} // 配置变更时唤醒循环重新计算下一次执行时间
	last    RenewResult
	lastAt  int64
	running bool
	onEvent func(store.RuntimeEvent)

	// locker 用于多实例协调。刷新是唯一不幂等的后台任务：
	// 两个实例同时刷新同一个 refresh_token，先刷的会把后刷的作废。
	locker Locker
}

// Locker 是分布式锁的最小接口，由 internal/cache 实现。
type Locker interface {
	Lock(ctx context.Context, name string, ttl time.Duration) (release func(), ok bool)
}

// SetLocker 注入分布式锁。传 nil 恢复单实例行为。
func (r *Renewer) SetLocker(l Locker) {
	r.mu.Lock()
	r.locker = l
	r.mu.Unlock()
}

// lockerOf 返回当前注入的锁实现。
func (r *Renewer) lockerOf() Locker {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.locker
}

// SetEventHook 注入运行事件钩子。
func (r *Renewer) SetEventHook(fn func(store.RuntimeEvent)) {
	r.mu.Lock()
	r.onEvent = fn
	r.mu.Unlock()
}

func (r *Renewer) Emit(ev store.RuntimeEvent) {
	r.mu.Lock()
	fn := r.onEvent
	r.mu.Unlock()
	if fn == nil {
		return
	}
	if ev.Scope == "" {
		ev.Scope = store.ScopeRenew
	}
	fn(ev)
}

// RenewConfig 是 Renewer 的运行参数。Window/Interval 为 0 表示关闭。
type RenewConfig struct {
	Window   time.Duration // 提前多久续期
	Interval time.Duration // 扫描间隔
	Gap      time.Duration // 账号间隔
}

func NewRenewer(ref *Refresher, cfg RenewConfig) *Renewer {
	return &Renewer{ref: ref, cfg: cfg, wake: make(chan struct{}, 1)}
}

// Update 热更新参数，并唤醒循环立即按新配置重排。
func (r *Renewer) Update(cfg RenewConfig) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Renewer) Config() RenewConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

// Status 供面板展示最近一轮结果。
func (r *Renewer) Status() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := r.cfg
	return map[string]any{
		"enabled":         cfg.Window > 0 && cfg.Interval > 0,
		"windowSeconds":   int(cfg.Window.Seconds()),
		"intervalMinutes": int(cfg.Interval.Minutes()),
		"gapMillis":       int(cfg.Gap.Milliseconds()),
		"checked":         r.last.Checked,
		"renewed":         r.last.Renewed,
		"failed":          r.last.Failed,
		"recovered":       r.last.Recovered,
		"lastRun":         r.lastAt,
		"running":         r.running,
	}
}

// Run 阻塞运行续期循环，直到 ctx 取消。参数变更通过 wake 立即生效。
func (r *Renewer) Run(ctx context.Context) {
	// 启动补跑：停机期间积累的临期号不必等满一个扫描周期。
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}
	_, _ = r.runOnce(ctx)

	for {
		cfg := r.Config()
		if cfg.Window <= 0 || cfg.Interval <= 0 {
			// 关闭状态：等配置变更通知，不做空转
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
				continue
			}
		}
		timer := time.NewTimer(cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.wake:
			timer.Stop()
		case <-timer.C:
			_, _ = r.runOnce(ctx)
		}
	}
}

// runOnce 执行一轮续期，返回本轮统计。ran=false 表示因为正在跑或已关闭而跳过。
func (r *Renewer) runOnce(ctx context.Context) (RenewResult, bool) {
	cfg := r.Config()
	if cfg.Window <= 0 || cfg.Interval <= 0 {
		return RenewResult{}, false
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return RenewResult{}, false
	}
	r.running = true
	r.mu.Unlock()

	res := r.ref.RenewDue(ctx, cfg.Window, cfg.Gap)
	res.At = time.Now().UnixMilli()

	r.mu.Lock()
	r.running = false
	r.last = res
	r.lastAt = res.At
	r.mu.Unlock()

	if res.Checked > 0 {
		log.Printf("[续期] 检查 %d，续期 %d，失败 %d，恢复 %d",
			res.Checked, res.Renewed, res.Failed, res.Recovered)
		level := store.LevelInfo
		if res.Failed > 0 {
			level = store.LevelWarn
		}
		r.Emit(store.RuntimeEvent{Level: level, Component: "renew", Message: "凭证续期一轮完成",
			Detail: fmt.Sprintf("检查 %d 个，续期 %d 个，失败 %d 个，恢复 %d 个",
				res.Checked, res.Renewed, res.Failed, res.Recovered)})
	}
	return res, true
}

// RunOnce 供面板「立即续期」按钮同步触发一轮。
// 已有续期在跑时返回 running=false，让调用方提示用户稍等，而不是把上一轮的旧数字当成结果。
func (r *Renewer) RunOnce(ctx context.Context) (RenewResult, bool) {
	return r.runOnce(ctx)
}

// RenewDue 主动续期：把 window 内就要到期的号统一刷新一遍。
//
// 为什么需要它：转发的刷新是惰性的，只在有请求打到这个号时才检查。池子大了以后
// 冷门号可能连续几十天不被选中，token（30 天）到期后它就从号池里消失了，而且
// 之后连 401 的机会都没有——不会再触发刷新。主动续期让每个号在到期前都续上。
//
// 失败不改变账号状态：续期是预防性的，失败可能是网络抖动，下一轮会重试；
// 只有上游明确回 401（凭据真的失效）才把号标成 relogin。
func (r *Refresher) RenewDue(ctx context.Context, window time.Duration, gap time.Duration) RenewResult {
	var out RenewResult
	accounts, err := r.ST.ListAccounts(ctx)
	if err != nil {
		return out
	}
	deadline := time.Now().Add(window).UnixMilli()
	for _, a := range accounts {
		if ctx.Err() != nil {
			return out
		}
		if !a.Enabled && a.Status != store.StatusRelogin {
			continue
		}
		if a.TokenExp == 0 || a.TokenExp > deadline {
			continue
		}
		if strings.TrimSpace(a.Token) == "" && strings.TrimSpace(a.RefreshToken) == "" {
			continue
		}
		out.Checked++
		wasRelogin := a.Status == store.StatusRelogin
		if _, err := r.RefreshNow(ctx, a); err != nil {
			out.Failed++
			var ue *raccoon.UpstreamError
			if errors.As(err, &ue) && ue.Auth {
				// 凭据确实失效：标记出来，交给面板「失效账号」清理
				relogin := store.StatusRelogin
				reason := "凭证失效，需重新导入"
				_ = r.ST.UpdateAccount(ctx, a.ID, store.AccountPatch{
					Status: &relogin, CoolReason: &reason,
				})
			}
			continue
		}
		out.Renewed++
		if wasRelogin {
			out.Recovered++
		}
		if gap > 0 {
			select {
			case <-ctx.Done():
				return out
			case <-time.After(gap):
			}
		}
	}
	return out
}

func needsRefresh(acc *store.Account, window time.Duration) bool {
	if acc.TokenExp == 0 {
		return false // 解不出 exp 就当作可用，让上游 401 来定
	}
	return time.Now().Add(window).UnixMilli() >= acc.TokenExp
}

// identityOf 由账号行组装指纹。
func identityOf(acc *store.Account) raccoon.Identity {
	return raccoon.IdentityFor(
		acc.UID,
		acc.DeviceID,
		acc.OSVersion,
		acc.Platform,
		acc.ClientChannel,
		acc.OfficeIdentity,
		acc.Language,
	)
}
