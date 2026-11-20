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
func (p *Pool) Pick(ctx context.Context, model string, excluded map[string]bool) (*Lease, error) {
	accounts, err := p.st.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	p.mu.Lock()
	strategy := p.strategy
	creditPol := p.credit
	loads := make(map[string]int, len(accounts))
	for id, n := range p.inFlight {
		loads[id] = n
	}
	seqs := make(map[string]uint64, len(p.pickSeq))
	for id, n := range p.pickSeq {
		seqs[id] = n
	}
	p.mu.Unlock()

	type cand struct {
		acc    *store.Account
		cred   int64
		expire int64
		last   int64
		seq    uint64
		load   int
	}
	var cands []cand
	for _, a := range accounts {
		if !a.Enabled || excluded[a.ID] {
			continue
		}
		if a.Status != store.StatusOnline {
			continue
		}
		if strings.TrimSpace(a.Token) == "" {
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
		cands = append(cands, cand{
			acc:    a,
			cred:   creditRank(a),
			expire: a.ExpiresAt,
			last:   a.LastUsed,
			seq:    seqs[a.ID],
			load:   loads[a.ID],
		})
	}
	if len(cands) == 0 {
		return nil, nil
	}

	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
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
	nowMs := time.Now().UnixMilli()
	_ = p.st.UpdateAccount(ctx, chosen.ID, store.AccountPatch{LastUsed: &nowMs})
	chosen.LastUsed = nowMs
	return &Lease{Account: chosen}, nil
}

func (l *Lease) Release() {}

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
