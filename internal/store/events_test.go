package store

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// 错误详情按「最近发生」排：合并过的老事件（ts 早、last_ts 新）要排在前面，
// 否则面板点开看到的是一条早已不相关的旧错误。
func TestLatestEventsOrdersByLastSeen(t *testing.T) {
	ctx := context.Background()
	st := newEventStore(t)
	now := time.Now().UnixMilli()
	if _, err := st.db.Exec(
		`INSERT INTO runtime_events (ts, level, scope, component, account_id, message, detail, count, last_ts)
		 VALUES (?,?,?,?,?,?,?,?,?), (?,?,?,?,?,?,?,?,?)`,
		now-3600_000, LevelWarn, ScopeRelay, "engine", "acc1", "反复失败", "repeated", 9, now,
		now-60_000, LevelInfo, ScopeRelay, "engine", "acc1", "一次性", "once", 1, now-60_000,
	); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LatestEvents(ctx, "acc1", []string{ScopeRelay}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Message != "反复失败" {
		t.Fatalf("应按最后发生时间倒序，实际 %+v", rows)
	}
	// 老数据（last_ts=0）回落到 ts，不能因为缺失列值被排到最后。
	if _, err := st.db.Exec(
		`INSERT INTO runtime_events (ts, level, scope, component, account_id, message, detail, count, last_ts)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		now-10_000, LevelWarn, ScopeRelay, "engine", "acc1", "老数据", "legacy", 1, 0,
	); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.LatestEvents(ctx, "acc1", nil, 5)
	if len(rows) != 3 || rows[1].Message != "老数据" {
		t.Fatalf("last_ts=0 应回落到 ts 排序（排在 1 小时前的旧事件之前），实际 %+v", rows)
	}
}

// 只取指定来源：号池冷却事件不该混进「转发失败」现场列表。
func TestLatestEventsFiltersScope(t *testing.T) {
	ctx := context.Background()
	st := newEventStore(t)
	now := time.Now().UnixMilli()
	for _, e := range []RuntimeEvent{
		{TS: now, Level: LevelWarn, Scope: ScopePool, Component: "cooldown", AccountID: "acc1", Message: "停用账号（冷却）"},
		{TS: now, Level: LevelWarn, Scope: ScopeRelay, Component: "engine", AccountID: "acc1", Message: "上游失败：HTTP 403"},
	} {
		if err := st.AddEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.LatestEvents(ctx, "acc1", []string{ScopeRelay}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Scope != ScopeRelay {
		t.Fatalf("应只返回 relay 事件，实际 %+v", rows)
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

// detail 的超限截断要按字符算：中文事件详情很容易超过 8K 字符，
// 误当字节截会把汉字从中间切开，面板上显示成乱码。
func TestAddEventTruncatesDetailByRunes(t *testing.T) {
	ctx := context.Background()
	st := newEventStore(t)
	detail := strings.Repeat("上游风控拦截原因", 2000) // 中文，字节数远超 8K
	if err := st.AddEvent(ctx, RuntimeEvent{
		Level: LevelWarn, Scope: ScopeRelay, Component: "engine", AccountID: "acc1",
		Message: "上游失败：HTTP 403", Detail: detail,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LatestEvents(ctx, "acc1", []string{ScopeRelay}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应写入一条事件，实际 %d", len(rows))
	}
	got := []rune(rows[0].Detail)
	if len(got) > EventMaxDetail {
		t.Fatalf("detail 应按字符截断到 %d，实际 %d 个字符", EventMaxDetail, len(got))
	}
	if !utf8.ValidString(rows[0].Detail) {
		t.Fatal("截断后的 detail 必须是合法 UTF-8，不能把汉字切坏")
	}
}
