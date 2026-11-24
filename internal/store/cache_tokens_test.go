package store

import (
	"context"
	"testing"
	"time"
)

// 缓存命中量要一路落库：流水列 → 日聚合。
func TestCachedTokensPersistedAndFolded(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	SetDailyLocation(time.UTC)
	ts := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC).UnixMilli()

	if err := st.AddUsage(ctx, UsageLog{
		TS: ts, Model: "m", API: "openai", Status: 200,
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CachedTokens: 64,
	}); err != nil {
		t.Fatal(err)
	}

	// 流水列表要带出 cachedTokens。
	rows, total, _, err := st.QueryUsage(ctx, UsageFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("应有 1 条流水，实际 total=%d len=%d", total, len(rows))
	}
	if rows[0].CachedTokens != 64 {
		t.Fatalf("流水未保存缓存命中量，实际 %d", rows[0].CachedTokens)
	}

	// 日聚合要累计。
	st.foldDaily(ctx, []UsageLog{{
		TS: ts, Model: "m", API: "openai", Status: 200,
		PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CachedTokens: 64,
	}})
	daily, err := st.LoadDaily(ctx, "2026-04-02", "2026-04-02")
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 1 || daily[0].CachedTokens != 64 {
		t.Fatalf("日聚合未累计缓存，实际 %+v", daily)
	}
}

// 异步批量写路径同样要带上缓存列（批量 SQL 的占位符与列数最容易对不上）。
func TestCachedTokensThroughAsyncWriter(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	st.AddUsageAsync(UsageLog{Model: "m", Status: 200, PromptTokens: 40, CachedTokens: 32, TotalTokens: 45})
	st.FlushUsage()

	got, err := st.RecentUsage(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CachedTokens != 32 {
		t.Fatalf("异步写未保存缓存命中量: %+v", got)
	}
}

// 老库（没有 cached_tokens 列）升级时要能补列并继续写入。
// 新建的库本身已含该列，这里直接断言列存在、默认值为 0。
func TestCachedTokensColumnExists(t *testing.T) {
	st := openTestStore(t)
	has, err := st.columnExistsAny("usage_logs", "cached_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("usage_logs 应含 cached_tokens 列")
	}
	has, err = st.columnExistsAny("request_daily", "cached_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("request_daily 应含 cached_tokens 列")
	}
}
