package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openStoreAnyDialect(t, "test-key", false)
}

// 只删除早于截止时间的流水，边界内的一条都不能动。
func TestPurgeUsageBefore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().UnixMilli()
	old := now - 40*24*3600*1000
	recent := now - 1*24*3600*1000

	_ = s.AddUsage(ctx, UsageLog{TS: old, Model: "old"})
	_ = s.AddUsage(ctx, UsageLog{TS: recent, Model: "recent"})

	cutoff := now - 30*24*3600*1000
	n, err := s.PurgeUsageBefore(ctx, cutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应删 1 条，实际 %d", n)
	}
	left, _ := s.RecentUsage(ctx, 10)
	if len(left) != 1 || left[0].Model != "recent" {
		t.Fatalf("保留的应是 recent，实际 %+v", left)
	}
}

// 分批删除：limit 决定一次最多删多少条。
func TestPurgeUsageBeforeRespectsLimit(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	old := time.Now().AddDate(0, 0, -60).UnixMilli()
	for i := 0; i < 5; i++ {
		_ = s.AddUsage(ctx, UsageLog{TS: old})
	}
	n, err := s.PurgeUsageBefore(ctx, time.Now().AddDate(0, 0, -30).UnixMilli(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("limit=2 时一次应删 2 条，实际 %d", n)
	}
}

func TestPurgeCheckinBefore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().UnixMilli()
	_ = s.AddCheckin(ctx, CheckinRecord{AccountID: "a", Day: "2020-01-01", OK: true, At: now - 100*24*3600*1000})
	_ = s.AddCheckin(ctx, CheckinRecord{AccountID: "b", Day: "today", OK: true, At: now})

	n, err := s.PurgeCheckinBefore(ctx, now-30*24*3600*1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应删 1 条旧签到，实际 %d", n)
	}
	_, earliest, err := s.CheckinStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if earliest < now-1000 {
		t.Fatalf("剩下的应是最新那条，earliest=%d", earliest)
	}
}

// 分页：按 id 倒序取，页大小与 offset 正确，total 不受分页影响。
func TestQueryUsagePagination(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		_ = s.AddUsage(ctx, UsageLog{TS: now + int64(i), Model: "m" + strconv.Itoa(i), Status: 200})
	}
	rows, total, failed, err := s.QueryUsage(ctx, UsageFilter{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 || failed != 0 {
		t.Fatalf("total/failed 应为 5/0，实际 %d/%d", total, failed)
	}
	if len(rows) != 2 {
		t.Fatalf("第 2 页应有 2 条，实际 %d", len(rows))
	}
	if rows[0].Model != "m2" || rows[1].Model != "m1" {
		t.Fatalf("第 2 页应是 m2,m1，实际 %s,%s", rows[0].Model, rows[1].Model)
	}
}

// 筛选：状态、时间窗、账号、模型、关键字都要真正进 SQL。
func TestQueryUsageFilters(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().UnixMilli()
	_ = s.AddUsage(ctx, UsageLog{TS: now - 2000, AccountID: "acc-1", Model: "gpt-a", Status: 200})
	_ = s.AddUsage(ctx, UsageLog{TS: now - 1000, AccountID: "acc-2", Model: "gpt-b", Status: 401, Error: "authorization_verify_error"})

	cases := []struct {
		name string
		f    UsageFilter
		want int64
	}{
		{"仅失败", UsageFilter{Status: "fail"}, 1},
		{"仅成功", UsageFilter{Status: "ok"}, 1},
		{"按账号", UsageFilter{AccountID: "acc-2"}, 1},
		{"按模型", UsageFilter{Model: "gpt-a"}, 1},
		{"时间窗", UsageFilter{SinceMS: now - 1500}, 1},
		{"错误关键字", UsageFilter{Query: "verify"}, 1},
		{"无匹配", UsageFilter{Model: "不存在"}, 0},
	}
	for _, c := range cases {
		_, total, _, err := s.QueryUsage(ctx, c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if total != c.want {
			t.Fatalf("%s: 应 %d 条，实际 %d", c.name, c.want, total)
		}
	}
}

// failed 计数与 total 同源，不需要前端再算一遍。
func TestQueryUsageFailedCount(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().UnixMilli()
	for _, st := range []int{200, 401, 500, 429} {
		_ = s.AddUsage(ctx, UsageLog{TS: now, Status: st})
	}
	_, total, failed, err := s.QueryUsage(ctx, UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || failed != 3 {
		t.Fatalf("应 4 条其中 3 条失败，实际 %d/%d", total, failed)
	}
}

// 空表统计不能报错，earliest 应为 0。
func TestStatsOnEmptyTables(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if n, e, err := s.UsageStats(ctx); err != nil || n != 0 || e != 0 {
		t.Fatalf("空表统计应为 0/0：n=%d e=%d err=%v", n, e, err)
	}
	if n, e, err := s.CheckinStats(ctx); err != nil || n != 0 || e != 0 {
		t.Fatalf("空表统计应为 0/0：n=%d e=%d err=%v", n, e, err)
	}
}
