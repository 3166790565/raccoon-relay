package store

import (
	"context"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), "test-key", false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
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
