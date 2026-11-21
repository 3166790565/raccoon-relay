package pool

import (
	"context"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 上游给出重置时刻（20 分钟）时，模型级冷却要对齐它，而不是死等本地 3 分钟。
func TestCoolModelRetryAfterExtendsCooldown(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	ctx := context.Background()

	cool.CoolModelRetryAfter(ctx, "acc-1", "m1", "限流", 20*time.Minute)
	infos := cool.ModelCooldownsOf("acc-1")
	if len(infos) != 1 {
		t.Fatalf("应有一条模型冷却，实际 %d", len(infos))
	}
	remain := time.Until(time.UnixMilli(infos[0].Until))
	if remain < 19*time.Minute || remain > 21*time.Minute {
		t.Fatalf("冷却应对齐上游重置时刻（约 20 分钟），实际 %v", remain)
	}
}

// 上游只给了很短的时间（10 秒）时不能比本地阶梯更短：
// 否则号会反复进出冷却，每 10 秒撞一次上游。
func TestCoolModelRetryAfterNeverShorterThanLadder(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	cool.CoolModelRetryAfter(context.Background(), "acc-2", "m1", "限流", 10*time.Second)
	infos := cool.ModelCooldownsOf("acc-2")
	if len(infos) != 1 {
		t.Fatalf("应有一条模型冷却，实际 %d", len(infos))
	}
	remain := time.Until(time.UnixMilli(infos[0].Until))
	if remain < 2*time.Minute {
		t.Fatalf("10 秒的重置提示不应短于本地首档 3 分钟，实际 %v", remain)
	}
}

// 荒谬的远期提示要封顶 24 小时。
func TestCoolModelRetryAfterCapped(t *testing.T) {
	st := newStore(t)
	cool := NewCooldownBook(st, true)
	cool.CoolModelRetryAfter(context.Background(), "acc-3", "m1", "限流", 72*time.Hour)
	infos := cool.ModelCooldownsOf("acc-3")
	if len(infos) != 1 {
		t.Fatalf("应有一条模型冷却，实际 %d", len(infos))
	}
	remain := time.Until(time.UnixMilli(infos[0].Until))
	if remain > 24*time.Hour+time.Minute {
		t.Fatalf("冷却应封顶 24 小时，实际 %v", remain)
	}
}

// 优先级数值大的账号先被选中，且在两种策略下都成立。
func TestPickHonorsPriority(t *testing.T) {
	for _, strategy := range []string{StrategyLoadBalance, StrategyRoundRobin} {
		t.Run(strategy, func(t *testing.T) {
			st := newStore(t)
			cool := NewCooldownBook(st, true)
			p := New(st, strategy, cool)
			low := addAcc(t, st, "low", 100)
			_ = addAcc(t, st, "high", 100, func(a *store.Account) { a.Priority = 10 })
			lease, err := p.Pick(context.Background(), "", map[string]bool{})
			if err != nil || lease == nil {
				t.Fatalf("应选中账号，err=%v", err)
			}
			if lease.Account.ID == low.ID {
				t.Fatal("高优先级账号应先被选中")
			}
		})
	}
}
