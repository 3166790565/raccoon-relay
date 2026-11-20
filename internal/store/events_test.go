package store

import (
	"context"
	"testing"
	"time"
)

func newEventStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), "", true)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 同一句话在合并窗口内重复出现时只累加次数，不新增行；
// 超过窗口后再来一条才算新事件。
func TestAddEventMergesRepeats(t *testing.T) {
	ctx := context.Background()
	st := newEventStore(t)
	ev := RuntimeEvent{Level: LevelError, Scope: ScopeRelay, Component: "engine",
		AccountID: "acc1", Message: "上游失败：429 限流", Detail: "第一次"}
	for i := 0; i < 3; i++ {
		if err := st.AddEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	rows, sum, err := st.QueryEvents(ctx, EventFilter{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 1 || len(rows) != 1 {
		t.Fatalf("窗口内重复事件应合并为 1 行，实际 %d 行 / total=%d", len(rows), sum.Total)
	}
	if rows[0].Count != 3 {
		t.Fatalf("次数应为 3，实际 %d", rows[0].Count)
	}

	// 把这条事件的时间推到窗口之外，再写一次应产生新行。
	if _, err := st.db.Exec(`UPDATE runtime_events SET ts = ?`, time.Now().Add(-2*eventMergeWindow).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if _, sum, _ = st.QueryEvents(ctx, EventFilter{Page: 1, PageSize: 50}); sum.Total != 2 {
		t.Fatalf("窗口外的相同事件应新增一行，实际 total=%d", sum.Total)
	}
}

// 级别筛选按「至少到这个程度」：选警告要能同时看到错误。
func TestQueryEventsLevelAndProblem(t *testing.T) {
	ctx := context.Background()
	st := newEventStore(t)
	now := time.Now().UnixMilli()
	for _, e := range []RuntimeEvent{
		{TS: now, Level: LevelInfo, Scope: ScopeCheckin, Message: "签到成功"},
		{TS: now, Level: LevelWarn, Scope: ScopePool, Message: "冷却"},
		{TS: now, Level: LevelError, Scope: ScopeRelay, Message: "号池无可用账号"},
	} {
		if err := st.AddEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		f    EventFilter
		want int64
	}{
		{"全部", EventFilter{}, 3},
		{"信息", EventFilter{Level: LevelInfo}, 1},
		{"警告含错误", EventFilter{Level: LevelWarn}, 2},
		{"只看错误", EventFilter{Level: LevelError}, 1},
		{"只看异常", EventFilter{OnlyProblem: true}, 2},
		{"按来源", EventFilter{Scopes: []string{ScopeRelay, ScopePool}}, 2},
		{"关键字", EventFilter{Query: "签到"}, 1},
	}
	for _, c := range cases {
		_, sum, err := st.QueryEvents(ctx, c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if sum.Total != c.want {
			t.Fatalf("%s: 期望 %d 条，实际 %d", c.name, c.want, sum.Total)
		}
	}
}

func TestPurgeEventsBefore(t *testing.T) {
	ctx := context.Background()
	st := newEventStore(t)
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	if err := st.AddEvent(ctx, RuntimeEvent{TS: old, Message: "旧事件"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEvent(ctx, RuntimeEvent{Message: "新事件"}); err != nil {
		t.Fatal(err)
	}
	n, err := st.PurgeEventsBefore(ctx, time.Now().Add(-24*time.Hour).UnixMilli(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应删除 1 条，实际 %d", n)
	}
	if count, _, _ := st.EventStats(ctx); count != 1 {
		t.Fatalf("应剩 1 条，实际 %d", count)
	}
}
