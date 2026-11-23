package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 并发数为 N 时能拿到 N 个不同槽位，第 N+1 个失败。
func TestAcquireLeaseSlotConcurrency(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)
	now := time.Now().UnixMilli()

	slots := map[int]bool{}
	for i := 0; i < 3; i++ {
		slot, until, ok, err := st.AcquireLeaseSlot(ctx, a.ID, 3, 60_000, now)
		if err != nil || !ok {
			t.Fatalf("第 %d 个槽位应抢到: ok=%v err=%v", i+1, ok, err)
		}
		if until <= now {
			t.Fatalf("到期时刻应大于当前时刻，实际 %d", until)
		}
		if slots[slot] {
			t.Fatalf("槽位 %d 被重复发放", slot)
		}
		slots[slot] = true
	}
	if _, _, ok, err := st.AcquireLeaseSlot(ctx, a.ID, 3, 60_000, now); ok || err != nil {
		t.Fatalf("第四个槽位应被拒绝，实际 ok=%v err=%v", ok, err)
	}
}

// 释放一个槽位后可以重新抢到；释放做 CAS，不会把别人的租约抹掉。
func TestReleaseLeaseSlotCAS(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)
	now := time.Now().UnixMilli()

	slot, until, ok, err := st.AcquireLeaseSlot(ctx, a.ID, 1, 60_000, now)
	if err != nil || !ok {
		t.Fatalf("抢槽位失败: %v", err)
	}
	// 错的 expect：不应释放。
	if err := st.ReleaseLeaseSlot(ctx, a.ID, slot, until+1); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := st.AcquireLeaseSlot(ctx, a.ID, 1, 60_000, now); ok {
		t.Fatal("CAS 不匹配的释放不该腾出槽位")
	}
	// 正确 expect：释放成功，可再次抢占。
	if err := st.ReleaseLeaseSlot(ctx, a.ID, slot, until); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := st.AcquireLeaseSlot(ctx, a.ID, 1, 60_000, now); !ok {
		t.Fatal("释放后应能重新抢到槽位")
	}
}

// 多副本同时抢同一个槽位，只有一个能成功（主键 + UPDATE CAS 的原子性）。
func TestAcquireLeaseSlotAtomicAcrossReplicas(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)
	now := time.Now().UnixMilli()

	var wg sync.WaitGroup
	wins := int32(0)
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, ok, err := st.AcquireLeaseSlot(ctx, a.ID, 1, 60_000, now)
			if err == nil && ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("并发数为 1 时只应有一个请求抢到，实际 %d", wins)
	}
}

// 槽位到期后自动空出（进程崩溃场景）。
func TestLeaseSlotExpires(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)
	now := time.Now().UnixMilli()

	if _, _, ok, _ := st.AcquireLeaseSlot(ctx, a.ID, 1, 1, now); !ok {
		t.Fatal("抢槽位失败")
	}
	// 用「当前时刻」再抢：刚才那个 1ms 租约已过期。
	time.Sleep(3 * time.Millisecond)
	if _, _, ok, _ := st.AcquireLeaseSlot(ctx, a.ID, 1, 60_000, time.Now().UnixMilli()); !ok {
		t.Fatal("过期槽位应能被重新抢占")
	}
}

// 槽位占用要汇总回 accounts.lease_until，选号层只看这一个字段。
func TestLeaseSlotsRefreshAccountColumn(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)
	now := time.Now().UnixMilli()

	_, until, ok, _ := st.AcquireLeaseSlot(ctx, a.ID, 2, 60_000, now)
	if !ok {
		t.Fatal("抢槽位失败")
	}
	got, err := st.GetAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaseUntil != until {
		t.Fatalf("accounts.lease_until 应汇总为 %d，实际 %d", until, got.LeaseUntil)
	}

	if err := st.ClearLeaseSlots(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetAccount(ctx, a.ID)
	if got.LeaseUntil != 0 {
		t.Fatalf("清空槽位后 lease_until 应为 0，实际 %d", got.LeaseUntil)
	}
}

// 重启后 HydrateLeaseSlots 要把仍在占用的槽位盖回 accounts.lease_until。
func TestHydrateLeaseSlots(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)
	now := time.Now().UnixMilli()

	_, until, ok, _ := st.AcquireLeaseSlot(ctx, a.ID, 1, 60_000, now)
	if !ok {
		t.Fatal("抢槽位失败")
	}
	// 模拟「上一版只写了槽位表、accounts 列为空」的库。
	if _, err := st.exec(ctx, `UPDATE accounts SET lease_until = 0 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	st.invalidateAccounts()

	if err := st.HydrateLeaseSlots(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetAccount(ctx, a.ID)
	if got.LeaseUntil != until {
		t.Fatalf("启动恢复应写回 %d，实际 %d", until, got.LeaseUntil)
	}
}
