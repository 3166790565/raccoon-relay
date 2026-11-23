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

// 客户端取消（499）既不算成功也不算失败：它单列一档，不能污染失败数，
// 也不能落进 4xx 筛选——499 是请求方行为，不是错误。
func TestQueryUsageCanceledExcludedFromFailed(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now().UnixMilli()
	for _, st := range []int{200, 502, UsageStatusCanceled} {
		_ = s.AddUsage(ctx, UsageLog{TS: now, Status: st, Error: "context canceled"})
	}

	_, total, failed, err := s.QueryUsage(ctx, UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("全部状态应 3 条，实际 %d", total)
	}
	if failed != 1 {
		t.Fatalf("失败只该数 502，实际 %d（499 不能算失败）", failed)
	}

	cases := []struct {
		name   string
		filter string
		want   int64
	}{
		{"失败筛选不含 499", "fail", 1},
		{"4xx 筛选不含 499", "4xx", 0},
		{"499 单列一档", "canceled", 1},
		{"成功筛选只有 200", "ok", 1},
	}
	for _, c := range cases {
		_, n, _, err := s.QueryUsage(ctx, UsageFilter{Status: c.filter})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n != c.want {
			t.Fatalf("%s: 应 %d 条，实际 %d", c.name, c.want, n)
		}
	}
}

// 日聚合必须跳过 499：它不进请求数，否则报表成功率会被客户端取消长期压低。
func TestFoldDailySkipsCanceled(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	SetDailyLocation(time.UTC)
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	st.foldDaily(ctx, []UsageLog{
		{TS: ts, Model: "m", API: "openai", Status: 200, TotalTokens: 10},
		{TS: ts + 1, Model: "m", API: "openai", Status: UsageStatusCanceled, TotalTokens: 0},
	})
	daily, err := st.LoadDaily(ctx, "2026-05-01", "2026-05-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 1 {
		t.Fatalf("应有 1 天，实际 %d", len(daily))
	}
	if daily[0].Requests != 1 || daily[0].Successful != 1 || daily[0].Tokens != 10 {
		t.Fatalf("499 不应进日聚合，实际 %+v", daily[0])
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
