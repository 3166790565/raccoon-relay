package store

import (
	"context"
	"testing"
	"time"
)

func newStoreForTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 绑定在 TTL 内有效，过期后自动失效。
func TestSessionBindingExpires(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	if err := st.BindSession(ctx, "s1", "acc-1", "m", 60_000, now); err != nil {
		t.Fatal(err)
	}
	if acc, ok := st.BoundAccount(ctx, "s1", now+1000); !ok || acc != "acc-1" {
		t.Fatalf("有效期内应命中绑定，got=%q ok=%v", acc, ok)
	}
	if _, ok := st.BoundAccount(ctx, "s1", now+60_001); ok {
		t.Fatal("过期后不应命中")
	}
	if _, ok := st.BoundAccount(ctx, "   ", now); ok {
		t.Fatal("空会话键不应命中")
	}
}

// 解绑后立刻失效。
func TestSessionUnbind(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_ = st.BindSession(ctx, "s1", "acc-1", "m", 60_000, now)
	if err := st.UnbindSession(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.BoundAccount(ctx, "s1", now); ok {
		t.Fatal("解绑后不应命中")
	}
}

// 重复绑定会滚动续期并改指向。
func TestSessionBindRollsTTL(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_ = st.BindSession(ctx, "s1", "acc-1", "m", 60_000, now)
	_ = st.BindSession(ctx, "s1", "acc-2", "m", 120_000, now+30_000)

	acc, ok := st.BoundAccount(ctx, "s1", now+100_000)
	if !ok || acc != "acc-2" {
		t.Fatalf("重新绑定应改指向并续期，got=%q ok=%v", acc, ok)
	}
}

func TestPruneSessions(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_ = st.BindSession(ctx, "old", "acc-1", "m", 1000, now-10_000)
	_ = st.BindSession(ctx, "live", "acc-2", "m", 60_000, now)

	n, err := st.PruneSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应清理 1 条过期绑定，实际 %d", n)
	}
	if _, ok := st.BoundAccount(ctx, "live", now); !ok {
		t.Fatal("未过期的不应被清掉")
	}
}

// ---------- 租约 ----------

func leaseAccount(t *testing.T, st *Store) *Account {
	t.Helper()
	a := &Account{UID: "u1", Name: "u1", Token: "t", Status: StatusOnline, Enabled: true}
	if err := st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// 租约只能被抢一次，CAS 释放只放自己的。
func TestLeaseCAS(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)
	now := time.Now().UnixMilli()

	until1, err := st.AcquireLease(ctx, a.ID, 60_000, now)
	if err != nil || until1 == 0 {
		t.Fatalf("首次抢租约应成功: until=%d err=%v", until1, err)
	}
	if until2, _ := st.AcquireLease(ctx, a.ID, 60_000, now); until2 != 0 {
		t.Fatal("未过期时第二次抢租约应失败")
	}

	// 用错的 expect 释放不应生效（别人的租约不能被抹掉）。
	if err := st.ReleaseLease(ctx, a.ID, until1+1); err != nil {
		t.Fatal(err)
	}
	acc, err := st.GetAccount(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acc.LeaseUntil != until1 {
		t.Fatalf("CAS 不匹配的释放不应清掉他人租约，实际 %d（期望 %d）", acc.LeaseUntil, until1)
	}

	// 用正确的 expect 才释放成功。
	if err := st.ReleaseLease(ctx, a.ID, until1); err != nil {
		t.Fatal(err)
	}
	if acc, _ := st.GetAccount(ctx, a.ID); acc.LeaseUntil != 0 {
		t.Fatalf("匹配的释放应清零，实际 %d", acc.LeaseUntil)
	}
}

func TestLeaseRenewCAS(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)
	now := time.Now().UnixMilli()

	until, _ := st.AcquireLease(ctx, a.ID, 60_000, now)
	renewed, err := st.RenewLease(ctx, a.ID, 60_000, until, now+1000)
	if err != nil || renewed <= until {
		t.Fatalf("续期应把到期时刻往后推: %d -> %d err=%v", until, renewed, err)
	}
	// 指纹不匹配（模拟已被他人接管）时续期失败。
	if again, _ := st.RenewLease(ctx, a.ID, 60_000, until, now+2000); again != 0 {
		t.Fatal("指纹不匹配的续期应失败")
	}
}

// ---------- 退避 ----------

func TestBackoffStore(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)

	if b, err := st.BackoffOf(ctx, a.ID); err != nil || b.Until != 0 {
		t.Fatalf("初始应无退避记录: %+v err=%v", b, err)
	}
	if err := st.UpsertBackoff(ctx, AccountBackoff{AccountID: a.ID, Until: 12345, Reason: "502", Hits: 2}); err != nil {
		t.Fatal(err)
	}
	b, err := st.BackoffOf(ctx, a.ID)
	if err != nil || b.Until != 12345 || b.Hits != 2 {
		t.Fatalf("读回退避失败: %+v err=%v", b, err)
	}
	all, err := st.ListBackoffs(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("列退避失败: %v err=%v", all, err)
	}

	if err := st.ClearBackoff(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.BackoffOf(ctx, a.ID); b.Until != 0 {
		t.Fatal("清除后不应再有退避")
	}
}

// 删除账号时，退避与会话绑定一起清掉，避免孤儿行。
func TestDeleteAccountClearsAuxiliaryRows(t *testing.T) {
	st := newStoreForTest(t)
	ctx := context.Background()
	a := leaseAccount(t, st)
	now := time.Now().UnixMilli()
	_ = st.UpsertBackoff(ctx, AccountBackoff{AccountID: a.ID, Until: now + 60_000})
	_ = st.BindSession(ctx, "s1", a.ID, "m", 60_000, now)

	if err := st.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.BackoffOf(ctx, a.ID); b.Until != 0 {
		t.Fatal("删号后退避记录应一并清理")
	}
	if _, ok := st.BoundAccount(ctx, "s1", now); ok {
		t.Fatal("删号后会话绑定应一并清理")
	}
}
