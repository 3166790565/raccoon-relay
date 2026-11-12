package pool

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

type Pool struct {
	st       *store.Store
	strategy string
	cooldown *CooldownBook

	mu       sync.Mutex
	inFlight map[string]int
}

func New(st *store.Store, strategy string, cooldown *CooldownBook) *Pool {
	return &Pool{st: st, strategy: strategy, cooldown: cooldown, inFlight: map[string]int{}}
}

func (p *Pool) SetStrategy(s string) {
	p.mu.Lock()
	p.strategy = s
	p.mu.Unlock()
}

func (p *Pool) Strategy() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.strategy
}

// Lease 表示一次选号结果，用完必须 Release。
type Lease struct {
	Account *store.Account
}

func (p *Pool) acquireInFlight(id string) {
	p.mu.Lock()
	p.inFlight[id]++
	p.mu.Unlock()
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
	p.mu.Unlock()

	type cand struct {
		acc    *store.Account
		rank   int
		cred   int64
		expire int64
		last   int64
		prio   int64
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
		if model != "" && p.cooldown.IsModelCooled(a.ID, model) {
			continue
		}
		cands = append(cands, cand{
			acc:    a,
			rank:   bucketRank(a, now),
			cred:   creditRank(a),
			expire: a.ExpiresAt,
			last:   a.LastUsed,
			prio:   a.Priority,
		})
	}
	if len(cands) == 0 {
		return nil, nil
	}

	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		var d int
		switch strategy {
		case "round_robin":
			d = cmpInt(a.last, b.last)
		case "credit_first":
			d = cmpBig(a.cred, b.cred)
		case "expire_first":
			if d = cmpExpire(a.expire, b.expire); d == 0 {
				d = cmpBig(a.cred, b.cred)
			}
		case "manual":
			d = cmpInt(a.prio, b.prio)
		default: // optimal
			if d = cmpInt(int64(a.rank), int64(b.rank)); d == 0 {
				if d = cmpBig(a.cred, b.cred); d == 0 {
					d = cmpExpire(a.expire, b.expire)
				}
			}
		}
		if d != 0 {
			return d < 0
		}
		if d = cmpInt(a.last, b.last); d != 0 {
			return d < 0
		}
		return a.acc.ID < b.acc.ID
	})

	chosen := cands[0].acc
	p.acquireInFlight(chosen.ID)
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

func bucketRank(a *store.Account, now int64) int {
	kind := gatewayBillingKind(a)
	if kind == "free" {
		return 0
	}
	if a.ExpiresAt > now {
		return 1
	}
	if kind == "permanent" {
		return 2
	}
	return 3
}

func gatewayBillingKind(a *store.Account) string {
	gw, ok := a.Meta["gateway"].(map[string]any)
	if !ok {
		return ""
	}
	k, _ := gw["billingKind"].(string)
	return k
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
