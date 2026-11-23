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

// 选号策略只有两种：负载均衡（默认）与轮询。
const (
	StrategyLoadBalance = "load_balance"
	StrategyRoundRobin  = "round_robin"
)

// NormalizeStrategy 把历史策略名折叠到现行两种。
// 旧版本有 optimal / credit_first / manual / expire_first，升级后环境下发的旧值
// 不应该让服务起不来，统一按负载均衡处理。
func NormalizeStrategy(s string) (string, bool) {
	switch strings.TrimSpace(s) {
	case StrategyRoundRobin:
		return StrategyRoundRobin, true
	case StrategyLoadBalance:
		return StrategyLoadBalance, true
	case "", "optimal", "credit_first", "manual", "expire_first":
		return StrategyLoadBalance, true
	}
	return "", false
}

// CreditPolicy 是「积分过低暂停、回升解除」的阈值规则。
// Enabled 为 false 时完全不生效。
type CreditPolicy struct {
	Enabled     bool
	StopBelow   int64 // 积分 <= StopBelow 时暂停
	ResumeAbove int64 // 积分 >= ResumeAbove 时解除
}

// Pauses 判断已知余额是否该暂停。未知余额（负数）不参与判断。
func (p CreditPolicy) Pauses(credits int64) bool {
	return p.Enabled && credits >= 0 && credits <= p.StopBelow
}

// Resumes 判断已知余额是否可以解除低积分暂停。
func (p CreditPolicy) Resumes(credits int64) bool {
	return !p.Enabled || credits < 0 || credits >= p.ResumeAbove
}

// StillPaused 判断一条「低积分暂停」是否还没到该解除的时候。
// StopBelow 与 ResumeAbove 相等时 Pauses 和 Resumes 会同时为真，
// 这里要求余额严格高于暂停阈值，避免状态在阈值上反复翻转。
func (p CreditPolicy) StillPaused(credits int64) bool {
	return p.Enabled && credits >= 0 && credits >= p.ResumeAbove && credits <= p.StopBelow
}

type Pool struct {
	st       *store.Store
	strategy string
	cooldown *CooldownBook
	credit   CreditPolicy

	mu       sync.Mutex
	inFlight map[string]int
	// pickSeq / pickTick 给轮询提供进程内的单调序号。
	// LastUsed 只到毫秒，连续几个请求落在同一毫秒里会打平，轮询就会退化成固定选同一个号。
	pickSeq  map[string]uint64
	pickTick uint64

	// preferID 是本轮请求「最好用这个号」的提示（会话粘性绑定 / 客户端指定）。
	// 传入时它排到候选队首，失败换号时由引擎清空，避免一直撞同一个号。
	preferID string
	// lease 是单号在途租约配置；held 是本进程当前持有的租约槽位。
	lease LeaseConfig
	held  map[string]map[int]*leaseState
	// backoff 是失败后的换号退避参数。
	backoff BackoffPolicy
	// now 供测试注入时钟。nil 用 time.Now。
	now func() time.Time
}

// SetPrefer 设置本次请求优先使用的账号 id。传空串清除。
func (p *Pool) SetPrefer(accountID string) {
	p.mu.Lock()
	p.preferID = accountID
	p.mu.Unlock()
}

// Prefer 返回当前优先账号。
func (p *Pool) Prefer() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.preferID
}

// SetClock 供测试注入时钟。
func (p *Pool) SetClock(fn func() time.Time) {
	p.mu.Lock()
	p.now = fn
	p.mu.Unlock()
}

func (p *Pool) clock() time.Time {
	p.mu.Lock()
	fn := p.now
	p.mu.Unlock()
	if fn == nil {
		return time.Now()
	}
	return fn()
}

func New(st *store.Store, strategy string, cooldown *CooldownBook) *Pool {
	if s, ok := NormalizeStrategy(strategy); ok {
		strategy = s
	} else {
		strategy = StrategyLoadBalance
	}
	return &Pool{
		st: st, strategy: strategy, cooldown: cooldown,
		inFlight: map[string]int{}, pickSeq: map[string]uint64{},
		lease: DefaultLeaseConfig(), held: map[string]map[int]*leaseState{},
		backoff: DefaultBackoffPolicy(),
	}
}

func (p *Pool) SetStrategy(s string) {
	if v, ok := NormalizeStrategy(s); ok {
		s = v
	} else {
		return
	}
	p.mu.Lock()
	p.strategy = s
	p.mu.Unlock()
}

func (p *Pool) Strategy() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.strategy
}

func (p *Pool) SetCreditPolicy(pol CreditPolicy) {
	if pol.ResumeAbove < pol.StopBelow {
		pol.ResumeAbove = pol.StopBelow
	}
	p.mu.Lock()
	p.credit = pol
	p.mu.Unlock()
}

func (p *Pool) CreditPolicy() CreditPolicy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.credit
}

// Lease 表示一次选号结果，用完必须 Release。
type Lease struct {
	Account *store.Account
}

func (p *Pool) acquireInFlight(id string) int {
	p.mu.Lock()
	p.inFlight[id]++
	n := p.inFlight[id]
	p.mu.Unlock()
	return n
}

func (p *Pool) releaseInFlight(id string) {
	p.mu.Lock()
	if p.inFlight[id] > 0 {
		p.inFlight[id]--
	}
	p.mu.Unlock()
}

func (p *Pool) inFlightOf(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inFlight[id]
}

// Pick 选一个可用账号。excluded 是本轮已试过的 id；model 用于模型级冷却过滤。
// 返回 nil 表示全池无可选。
//
// 过滤顺序即「这个号现在能不能接活」：停用/非在线/无凭证/积分耗尽/到期/
// 积分阈值/模型级冷却/在途租约/退避窗口。
// 排序时优先账号（会话绑定或客户端指定）排第一，其余按策略排。
func (p *Pool) Pick(ctx context.Context, model string, excluded map[string]bool) (*Lease, error) {
	return p.pick(ctx, model, "", excluded)
}

// PickProvider 只在指定供应商的账号里选。provider 为空等价不限制。
// 日日新与小浣熊的模型不通用，按模型名分流后必须用这个入口，否则会拿
// deepseek-flash 去打小浣熊、或拿 raccoon-chat 去打日日新。
func (p *Pool) PickProvider(ctx context.Context, model, provider string, excluded map[string]bool) (*Lease, error) {
	return p.pick(ctx, model, provider, excluded)
}

func (p *Pool) pick(ctx context.Context, model, wantProvider string, excluded map[string]bool) (*Lease, error) {
	accounts, err := p.st.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	now := p.clock().UnixMilli()
	p.mu.Lock()
	strategy := p.strategy
	creditPol := p.credit
	prefer := p.preferID
	leases := p.lease
	loads := make(map[string]int, len(accounts))
	for id, n := range p.inFlight {
		loads[id] = n
	}
	seqs := make(map[string]uint64, len(p.pickSeq))
	for id, n := range p.pickSeq {
		seqs[id] = n
	}
	p.mu.Unlock()

	// 退避门闸一次读出：逐个查会变成 N 次往返。
	backoffs, _ := p.st.ListBackoffs(ctx)
	// 租约占用同理，一次聚合查询读出全部账号的已占槽位数。
	// 必须看库里的事实：槽位可能被别的副本占着，进程内计数看不见。
	var leaseUsed map[string]int
	if leases.Enabled {
		leaseUsed, _ = p.st.LeaseCounts(ctx, now)
	}

	type cand struct {
		acc    *store.Account
		cred   int64
		expire int64
		last   int64
		seq    uint64
		load   int
		prefer bool
	}
	var cands []cand
	for _, a := range accounts {
		if !a.Enabled || excluded[a.ID] {
			continue
		}
		if wantProvider != "" && aProvider(a) != wantProvider {
			continue
		}
		if a.Status != store.StatusOnline {
			continue
		}
		// 推理凭据：小浣熊是 access_token；日日新的 API Key 存在 Token 里，
		// 只有控制台账号密码时可以在首次转发时现场补建，所以这里允许空 Token。
		// 外部渠道（AutoClaw / CodeArts）的凭据在 Ext* 槽里，Token 列本就为空。
		if strings.TrimSpace(a.Token) == "" && !hasSenseNovaCreds(a) && !hasExternalCreds(a) {
			continue
		}
		if a.Credits == 0 && a.CreditsAt > 0 {
			continue
		}
		if a.ExpiresAt > 0 && a.ExpiresAt <= now {
			continue
		}
		if creditPol.Pauses(a.Credits) {
			continue
		}
		if model != "" && p.cooldown.IsModelCooled(a.ID, model) {
			continue
		}
		// 单号并发上限：该号在跑满后就跳过，让请求落到别的号上。
		// 0 表示不单独限制（跟随全局并发上限）。
		if a.MaxConcurrent > 0 && int64(loads[a.ID]) >= a.MaxConcurrent {
			continue
		}
		// 在途租约：同一个号最多 N 个并发槽位（N = 面板配置的租约并发数，
		// 1 即整号串行）。任一槽位被占时 lease_until > now。到期后自动失效，
		// 进程崩溃留下的残租约会回收。
		//
		// 用**库里**的已占槽位数判断满没满：它同时包含本进程与别的副本的占用。
		// 不能用进程内计数器（多副本看不见别人），也不能把本进程的占用再加一遍
		// （那样并发数 2 实际只放出 1 路）。选号与真正抢槽之间仍有窗口，
		// 抢不到时由 AcquireLeaseSlot 的 CAS 兜底，调用方换号即可。
		if leases.Enabled && leaseUsed[a.ID] >= leases.Concurrency {
			continue
		}
		// 退避门闸：刚失败过的号在窗口内不接新请求。
		if b, ok := backoffs[a.ID]; ok && b.Until > now {
			continue
		}
		cands = append(cands, cand{
			acc:    a,
			cred:   creditRank(a),
			expire: a.ExpiresAt,
			last:   a.LastUsed,
			seq:    seqs[a.ID],
			load:   loads[a.ID],
			prefer: prefer != "" && a.ID == prefer,
		})
	}
	if len(cands) == 0 {
		return nil, nil
	}

	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		// 会话绑定/客户端指定的号排最前：同一段对话尽量不跳号。
		// 这比优先级更硬——粘性选中就是为了不再换号，优先级只在不影响会话时生效。
		if a.prefer != b.prefer {
			return a.prefer
		}
		// 优先级最高且跨策略生效：数值大的先用（0 为默认，互不影响）。
		// 跑干几个号再切到备用号是运维上的常见需求，不该被选号策略覆盖。
		if a.acc.Priority != b.acc.Priority {
			return a.acc.Priority > b.acc.Priority
		}
		switch strategy {
		case StrategyRoundRobin:
			// 轮询：最久没被选中的优先，让请求均匀摊到每个号上。
			// 比较进程内递增序号而不是毫秒时间戳：同一毫秒的并发请求也要能错开。
			if a.seq != b.seq {
				return a.seq < b.seq
			}
		default: // load_balance
			// 负载均衡：当前在途请求最少者优先；同负载按最久未用；再按积分多者。
			if a.load != b.load {
				return a.load < b.load
			}
			if a.seq != b.seq {
				return a.seq < b.seq
			}
			if d := cmpInt(a.last, b.last); d != 0 {
				return d < 0
			}
			if d := cmpBig(a.cred, b.cred); d != 0 {
				return d < 0
			}
			if d := cmpExpire(a.expire, b.expire); d != 0 {
				return d < 0
			}
		}
		return a.acc.ID < b.acc.ID
	})

	chosen := cands[0].acc
	p.acquireInFlight(chosen.ID)
	p.mu.Lock()
	p.pickTick++
	p.pickSeq[chosen.ID] = p.pickTick
	p.mu.Unlock()
	_ = p.st.UpdateAccount(ctx, chosen.ID, store.AccountPatch{LastUsed: &now})
	chosen.LastUsed = now
	return &Lease{Account: chosen}, nil
}

func (l *Lease) Release() {}

// NextAvailableIn 返回「下一次可能有号可选」还要等多久，供换号时决定要不要等。
//
// Pick 选不到号不代表号池真的空了：号可能只是正被别的请求占着（在途租约）、
// 刚失败还在退避窗口里，或者并发上限暂时打满。这些情况下几百毫秒后就能空出来，
// 立刻放弃会把上一次的上游错误直接丢给调用方。反过来，号都不在线
// （cooling / exhausted / relogin）或都在模型级冷却里时，等也没用。
//
// 返回值 d 为 0 表示「现在就该有号」（调用方得到 nil 说明是别的原因）；
// ok=false 表示没有任何在线的号值得等。
func (p *Pool) NextAvailableIn(ctx context.Context, model, wantProvider string, excluded map[string]bool) (time.Duration, bool) {
	accounts, err := p.st.ListAccounts(ctx)
	if err != nil {
		return 0, false
	}
	now := p.clock().UnixMilli()
	p.mu.Lock()
	creditPol := p.credit
	leases := p.lease
	loads := make(map[string]int, len(p.inFlight))
	for id, n := range p.inFlight {
		loads[id] = n
	}
	p.mu.Unlock()
	backoffs, _ := p.st.ListBackoffs(ctx)
	var leaseUsed map[string]int
	if leases.Enabled {
		leaseUsed, _ = p.st.LeaseCounts(ctx, now)
	}

	waiting := false   // 存在「其余条件都满足」的号，只是并发上限暂时打满
	var bestWait int64 // 号池里最早空出来的时刻（相对 now 的毫秒）
	for _, a := range accounts {
		if !a.Enabled || excluded[a.ID] {
			continue
		}
		if wantProvider != "" && aProvider(a) != wantProvider {
			continue
		}
		if a.Status != store.StatusOnline {
			continue
		}
		if strings.TrimSpace(a.Token) == "" && !hasSenseNovaCreds(a) && !hasExternalCreds(a) {
			continue
		}
		// 与 Pick 的过滤保持一致：余额为 0、订阅过期、积分低于阈值不是
		// 「等一会儿就会空出来」，它们要等签到/续费等别的流程处理。
		if a.Credits == 0 && a.CreditsAt > 0 {
			continue
		}
		if a.ExpiresAt > 0 && a.ExpiresAt <= now {
			continue
		}
		if creditPol.Pauses(a.Credits) {
			continue
		}
		// 模型级冷却按分钟计，等它不划算。
		if model != "" && p.cooldown.IsModelCooled(a.ID, model) {
			continue
		}
		// 租约：只有槽位占满才算「等一会儿就会空出来」；没占满的号此刻就能多接一路。
		if leases.Enabled && leaseUsed[a.ID] >= leases.Concurrency {
			if d := a.LeaseUntil - now; bestWait == 0 || d < bestWait {
				bestWait = d
			}
			continue
		}
		if b, ok := backoffs[a.ID]; ok && b.Until > now {
			if d := b.Until - now; bestWait == 0 || d < bestWait {
				bestWait = d
			}
			continue
		}
		// 其余条件都满足：这个号要么现在就能选，要么只是并发上限暂时打满。
		// 打满时算不出结束时刻，按轮询重试（返回 0）。
		if a.MaxConcurrent > 0 && int64(loads[a.ID]) >= a.MaxConcurrent {
			waiting = true
			continue
		}
		return 0, true
	}
	if waiting {
		// 有号条件都满足、只是被并发上限打满：几百毫秒后轮询即可。
		return 0, true
	}
	if bestWait > 0 {
		return time.Duration(bestWait) * time.Millisecond, true
	}
	return 0, false
}

// PickProbe 是「号池抽干」时的兜底探测：在所有账号都因冷却/积分/凭证不可用时，
// 挑一个最早恢复的号强制试一次，让调用方拿到上游的真实错误（例如 429 的重置时间），
// 而不是我们推断出来的「没有可用账号」。返回 nil 表示连一个能探测的号都没有。
//
// 只放宽「状态与冷却」这一类条件：停用、无 token、订阅过期的号连试都不该试，
// 探测它们只会制造无意义的失败。
func (p *Pool) PickProbe(ctx context.Context, model string, excluded map[string]bool) *Lease {
	return p.pickProbe(ctx, model, "", excluded)
}

// PickProbeProvider 是带供应商过滤的 PickProbe。
func (p *Pool) PickProbeProvider(ctx context.Context, model, provider string, excluded map[string]bool) *Lease {
	return p.pickProbe(ctx, model, provider, excluded)
}

func (p *Pool) pickProbe(ctx context.Context, model, wantProvider string, excluded map[string]bool) *Lease {
	accounts, err := p.st.ListAccounts(ctx)
	if err != nil {
		return nil
	}
	now := time.Now().UnixMilli()
	var best *store.Account
	bestUntil := int64(0)
	for _, a := range accounts {
		if !a.Enabled || excluded[a.ID] {
			continue
		}
		if wantProvider != "" && aProvider(a) != wantProvider {
			continue
		}
		if strings.TrimSpace(a.Token) == "" && !hasSenseNovaCreds(a) && !hasExternalCreds(a) {
			continue
		}
		if a.ExpiresAt > 0 && a.ExpiresAt <= now {
			continue
		}
		switch a.Status {
		case store.StatusOnline, store.StatusCooling, store.StatusExhausted:
		default:
			continue // relogin 等：重试不会成功
		}
		// 订阅到期/积分阈值的判断与其他路径一致，避免探测注定失败的号
		if p.CreditPolicy().Pauses(a.Credits) {
			continue
		}
		// 冷却中的号按恢复时刻排序，最早恢复的最可能刚刚恢复可用
		if best == nil || (a.CoolUntil > 0 && (bestUntil == 0 || a.CoolUntil < bestUntil)) {
			best, bestUntil = a, a.CoolUntil
		}
	}
	if best == nil {
		return nil
	}
	p.acquireInFlight(best.ID)
	p.mu.Lock()
	p.pickTick++
	p.pickSeq[best.ID] = p.pickTick
	p.mu.Unlock()
	return &Lease{Account: best}
}

// ReleaseLease 由调用方在请求结束后调用，释放并发计数。
func (p *Pool) ReleaseLease(l *Lease) {
	if l == nil || l.Account == nil {
		return
	}
	p.releaseInFlight(l.Account.ID)
}

// ApplyCreditPolicy 让账号状态与积分阈值规则保持一致。
//
//   - 已知余额 <= StopBelow 且在线 → exhausted + CoolUntil 0（等签到/余额回升）
//   - 处于「低积分暂停」（exhausted 且 CoolUntil == 0）且余额 >= ResumeAbove → online
//   - 规则被关闭时，解除所有低积分暂停
//
// 402 造成的 exhausted 带未来的 CoolUntil，不会被这里误放出来。
func (p *Pool) ApplyCreditPolicy(ctx context.Context) {
	pol := p.CreditPolicy()
	accounts, err := p.st.ListAccounts(ctx)
	if err != nil {
		return
	}
	exhausted := store.StatusExhausted
	online := store.StatusOnline
	zero := int64(0)
	for _, a := range accounts {
		paused := a.Status == exhausted && a.CoolUntil == 0
		switch {
		case pol.Pauses(a.Credits):
			if a.Status == store.StatusOnline {
				reason := fmt.Sprintf("积分低于阈值 %d，等待签到/余额恢复", pol.StopBelow)
				_ = p.st.UpdateAccount(ctx, a.ID, store.AccountPatch{
					Status: &exhausted, CoolUntil: &zero, CoolReason: &reason,
				})
			}
		case paused && !pol.StillPaused(a.Credits) && pol.Resumes(a.Credits):
			// StillPaused 挡住「暂停阈值 = 解除阈值」的情况：那样 Pauses 与 Resumes
			// 会同时为真，状态每分钟翻转一次、白写一次库。等余额真正涨过阈值再放。
			empty := ""
			_ = p.st.UpdateAccount(ctx, a.ID, store.AccountPatch{
				Status: &online, CoolUntil: &zero, CoolReason: &empty,
			})
		}
	}
}

// aProvider 返回账号的供应商，空值按小浣熊。
func aProvider(a *store.Account) string {
	if a == nil || a.Provider == "" {
		return store.ProviderRaccoon
	}
	return a.Provider
}

// hasSenseNovaCreds 判断日日新账号是否具备可用的推理路径：已有 API Key，
// 或有控制台登录票 / 账号密码（可现场换 Key）。
func hasSenseNovaCreds(a *store.Account) bool {
	if !a.IsSenseNova() {
		return false
	}
	if strings.TrimSpace(a.Token) != "" {
		return true
	}
	return (strings.TrimSpace(a.SNUsername) != "" && a.SNPassword != "") ||
		strings.TrimSpace(a.SNToken) != ""
}

// hasExternalCreds 判断外部渠道账号是否有可用凭据（主凭据或续期材料之一）。
func hasExternalCreds(a *store.Account) bool {
	if !a.IsExternal() {
		return false
	}
	return strings.TrimSpace(a.ExtToken) != "" ||
		strings.TrimSpace(a.ExtRefreshToken) != "" ||
		strings.TrimSpace(a.ExtExtra) != ""
}

func creditRank(a *store.Account) int64 {
	if a.Credits < 0 {
		return 1 << 62 // 未知积分视为最多
	}
	return a.Credits
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpBig 让「积分多者优先」：返回负数表示 a 排前。
func cmpBig(a, b int64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

func cmpExpire(a, b int64) int {
	if a == 0 {
		a = 1 << 62
	}
	if b == 0 {
		b = 1 << 62
	}
	return cmpInt(a, b)
}
