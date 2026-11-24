package store

import (
	"context"
	"testing"
	"time"
)

func seedAcc(t *testing.T, s *Store, uid string, mutate func(*Account)) *Account {
	t.Helper()
	a := &Account{
		UID: uid, Name: uid, Token: "tok-" + uid, Status: StatusOnline,
		Credits: -1, DeviceID: "dev-" + uid, Language: "zh", Enabled: true,
	}
	if mutate != nil {
		mutate(a)
	}
	if err := s.CreateAccount(context.Background(), a); err != nil {
		t.Fatalf("建号 %s 失败: %v", uid, err)
	}
	return a
}

// 只有 relogin 与已过期算失效；冷却、积分耗尽都还能自动恢复，不能删。
func TestListDeadAccounts(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().UnixMilli()

	seedAcc(t, s, "alive", nil)
	seedAcc(t, s, "cooling", func(a *Account) {
		a.Status = StatusCooling
		a.CoolUntil = now + 60_000
	})
	seedAcc(t, s, "exhausted", func(a *Account) { a.Status = StatusExhausted })
	seedAcc(t, s, "relogin", func(a *Account) { a.Status = StatusRelogin })
	seedAcc(t, s, "expired", func(a *Account) { a.ExpiresAt = now - 1000 })
	seedAcc(t, s, "future", func(a *Account) { a.ExpiresAt = now + 24*3600*1000 })

	dead, err := s.ListDeadAccounts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range dead {
		got[d.UID] = d.Reason
	}
	want := map[string]string{"relogin": "凭证失效", "expired": "订阅已过期"}
	if len(got) != len(want) {
		t.Fatalf("应只列出 %d 个失效账号，实际 %v", len(want), got)
	}
	for uid, reason := range want {
		if got[uid] != reason {
			t.Fatalf("%s 的失效原因应为 %q，实际 %q", uid, reason, got[uid])
		}
	}
}

// 已下线渠道的历史账号要算失效：它们不参与选号，留在列表里只会让用户
// 以为这是一台还能用的小浣熊号。清理入口应当能把它们一起收走。
func TestDeadAccountsIncludeRemovedProviders(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().UnixMilli()

	seedAcc(t, s, "rac", nil)
	seedAcc(t, s, "ext", func(a *Account) {
		a.Provider = ProviderAutoClaw
		a.Status = StatusOnline
	})
	seedAcc(t, s, "ca", func(a *Account) {
		a.Provider = ProviderCodeArts
		a.Status = StatusOnline
	})

	dead, err := s.ListDeadAccounts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range dead {
		got[d.UID] = d.Reason
	}
	if len(got) != 2 || got["ext"] != "渠道已下线" || got["ca"] != "渠道已下线" {
		t.Fatalf("已下线渠道应计入失效，实际 %v", got)
	}
	if _, ok := got["rac"]; ok {
		t.Fatal("正常小浣熊号不该被列为失效")
	}

	// 面板上的名字不能回落成「小浣熊」，否则会被当成可用号。
	for _, uid := range []string{"ext", "ca"} {
		list, _ := s.ListAccounts(ctx)
		for _, a := range list {
			if a.UID == uid && a.ProviderName() == "小浣熊" {
				t.Fatalf("%s 的显示名回落成了小浣熊", uid)
			}
		}
	}
}

// 删除必须连带清掉冷却与签到记录，不能留下孤儿行。
func TestPurgeDeadAccountsCleansDependents(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().UnixMilli()

	dead := seedAcc(t, s, "dead", func(a *Account) { a.Status = StatusRelogin })
	alive := seedAcc(t, s, "alive", nil)

	if err := s.UpsertModelCooldown(ctx, ModelCooldown{
		AccountID: dead.ID, Model: "m1", Until: now + 60_000, Reason: "x",
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.AddCheckin(ctx, CheckinRecord{AccountID: dead.ID, Day: "2026-01-01", OK: false})
	_ = s.AddCheckin(ctx, CheckinRecord{AccountID: alive.ID, Day: "2026-01-01", OK: true})

	n, err := s.PurgeDeadAccounts(ctx, []string{dead.ID})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应删 1 个，实际 %d", n)
	}

	if got, _ := s.GetAccount(ctx, dead.ID); got != nil {
		t.Fatal("失效账号应已删除")
	}
	if got, _ := s.GetAccount(ctx, alive.ID); got == nil {
		t.Fatal("正常账号不该被误删")
	}

	var cools, checkins int
	if err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_cooldowns WHERE account_id = ?`, dead.ID).Scan(&cools); err != nil {
		t.Fatal(err)
	}
	if cools != 0 {
		t.Fatalf("冷却记录应一并删除，实际剩 %d 条", cools)
	}
	if err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkin_history WHERE account_id = ?`, dead.ID).Scan(&checkins); err != nil {
		t.Fatal(err)
	}
	if checkins != 0 {
		t.Fatalf("签到记录应一并删除，实际剩 %d 条", checkins)
	}
	// 没有命中失效条件的号，数据必须原样保留
	if err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkin_history WHERE account_id = ?`, alive.ID).Scan(&checkins); err != nil {
		t.Fatal(err)
	}
	if checkins != 1 {
		t.Fatalf("正常账号的签到记录不该被删，实际剩 %d 条", checkins)
	}
}

// 空清单是合法输入（没有失效账号），不做任何事也不报错。
func TestPurgeDeadAccountsEmpty(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAcc(t, s, "alive", nil)
	n, err := s.PurgeDeadAccounts(ctx, nil)
	if err != nil {
		t.Fatalf("空清单不该报错: %v", err)
	}
	if n != 0 {
		t.Fatalf("空清单应删 0 个，实际 %d", n)
	}
}
