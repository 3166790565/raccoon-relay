package store

import (
	"context"
	"testing"
	"time"
)

func seedAccount(t *testing.T, s *Store, uid string, enabled bool) *Account {
	t.Helper()
	a := &Account{
		UID: uid, Name: uid, Token: "tok", Status: StatusOnline,
		Enabled: enabled, Credits: -1, Meta: map[string]any{},
	}
	if err := s.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// 签到中心账号行要显示「今日最后一条记录」：先失败再成功时，行上是成功，
// 而且通过 last_checkin_day 判已签。
func TestCheckinCenterAccountsUsesLatestRecord(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	today := time.Now().Format("2006-01-02")
	a := seedAccount(t, s, "pu-1", true)

	_ = s.AddCheckin(ctx, CheckinRecord{
		AccountID: a.ID, Day: today, Action: "checkin", Source: CheckinSourceAuto,
		OK: false, Message: "上游抖动",
	})
	_ = s.AddCheckin(ctx, CheckinRecord{
		AccountID: a.ID, Day: today, Action: "checkin", Source: CheckinSourceManual,
		OK: true, Already: true, Message: "今日已领取过",
	})
	_ = s.UpdateAccount(ctx, a.ID, AccountPatch{LastCheckinDay: &today})

	rows, err := s.CheckinCenterAccounts(ctx, today)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应返回 1 个账号，实际 %d", len(rows))
	}
	r := rows[0]
	if !r.HasToday || !r.TodayOK || !r.TodayAlready {
		t.Fatalf("应取今日最新的成功记录，实际 %+v", r)
	}
	if r.TodaySource != CheckinSourceManual {
		t.Fatalf("来源应取最新记录的 manual，实际 %q", r.TodaySource)
	}
}

// 没有今日记录的号不能被误判为「有今日记录」。
func TestCheckinCenterAccountsNoTodayRecord(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	today := time.Now().Format("2006-01-02")
	a := seedAccount(t, s, "pu-2", true)
	_ = s.AddCheckin(ctx, CheckinRecord{
		AccountID: a.ID, Day: "2020-01-01", Action: "checkin", OK: true,
	})

	rows, _ := s.CheckinCenterAccounts(ctx, today)
	if len(rows) != 1 {
		t.Fatalf("应返回 1 个账号，实际 %d", len(rows))
	}
	if rows[0].HasToday {
		t.Fatal("只有历史记录时 HasToday 应为 false")
	}
}

// 来源筛选把定时与手动分开，结果筛选区分领取成功与今日已领。
func TestQueryCheckinsFilters(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := seedAccount(t, s, "pu-3", true)
	day := "2026-01-02"

	_ = s.AddCheckin(ctx, CheckinRecord{AccountID: a.ID, Day: day, Action: "checkin", Source: CheckinSourceAuto, OK: true, Already: false, Credit: 300})
	_ = s.AddCheckin(ctx, CheckinRecord{AccountID: a.ID, Day: day, Action: "checkin", Source: CheckinSourceManual, OK: true, Already: true})
	_ = s.AddCheckin(ctx, CheckinRecord{AccountID: a.ID, Day: day, Action: "checkin", Source: CheckinSourceManual, OK: false, Message: "失败"})

	rows, sum, err := s.QueryCheckins(ctx, CheckinFilter{Source: CheckinSourceManual, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 2 || len(rows) != 2 {
		t.Fatalf("手动来源应 2 条，实际 total=%d rows=%d", sum.Total, len(rows))
	}
	if sum.Failed != 1 {
		t.Fatalf("失败数应为 1，实际 %d", sum.Failed)
	}

	rows, sum, _ = s.QueryCheckins(ctx, CheckinFilter{Result: "claimed", Page: 1, PageSize: 10})
	if sum.Total != 1 || len(rows) != 1 || rows[0].Credit != 300 {
		t.Fatalf("claimed 应只命中真正领到积分的那条，实际 total=%d %+v", sum.Total, rows)
	}

	rows, sum, _ = s.QueryCheckins(ctx, CheckinFilter{Result: "already", Page: 1, PageSize: 10})
	if sum.Total != 1 || rows[0].Already != true {
		t.Fatalf("already 应只命中今日已领那条，实际 total=%d", sum.Total)
	}
}

// 记录列表按时间倒序：最新的排在最前，面板一眼看到最近一次结果。
func TestQueryCheckinsOrdersNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := seedAccount(t, s, "pu-4", true)
	base := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		_ = s.AddCheckin(ctx, CheckinRecord{
			AccountID: a.ID, Day: "2026-01-03", Action: "checkin",
			OK: true, Message: string(rune('A' + i)), At: base + int64(i),
		})
	}
	rows, _, _ := s.QueryCheckins(ctx, CheckinFilter{Page: 1, PageSize: 10})
	if len(rows) != 3 || rows[0].Message != "C" {
		t.Fatalf("应按 id 倒序，首条应为最新的 C，实际 %+v", rows)
	}
}
