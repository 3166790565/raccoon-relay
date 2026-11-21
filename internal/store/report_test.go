package store

import (
	"context"
	"testing"
	"time"
)

func TestFoldDailyAggregates(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	SetDailyLocation(time.UTC)
	ts := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC).UnixMilli()
	batch := []UsageLog{
		{TS: ts, Model: "m1", API: "openai", Status: 200, TotalTokens: 100, PromptTokens: 40, CompletionTokens: 60},
		{TS: ts + 1000, Model: "m1", API: "openai", Status: 200, TotalTokens: 50, PromptTokens: 20, CompletionTokens: 30},
		{TS: ts + 2000, Model: "m2", API: "openai", Status: 429, TotalTokens: 0},
		{TS: time.Date(2026, 3, 11, 1, 0, 0, 0, time.UTC).UnixMilli(), Model: "m2", API: "openai", Status: 502, TotalTokens: 0},
	}
	st.foldDaily(ctx, batch)

	daily, err := st.LoadDaily(ctx, "2026-03-01", "2026-03-31")
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 2 {
		t.Fatalf("应有 2 天，实际 %d", len(daily))
	}
	if daily[0].Day != "2026-03-10" || daily[0].Requests != 3 || daily[0].Successful != 2 || daily[0].Tokens != 150 {
		t.Fatalf("3-10 聚合不对: %+v", daily[0])
	}
	if daily[1].Day != "2026-03-11" || daily[1].Successful != 0 {
		t.Fatalf("3-11 聚合不对: %+v", daily[1])
	}
	if daily[0].PromptTokens != 60 || daily[0].Completion != 90 {
		t.Fatalf("3-10 输入/输出拆分不对: %+v", daily[0])
	}

	models, err := st.LoadDailyModels(ctx, "2026-03-01", "2026-03-31")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, m := range models {
		got[m.Key] += m.Tokens
	}
	if got["m1"] != 150 {
		t.Fatalf("m1 应累计 150 token，实际 %d", got["m1"])
	}

	// 协议分解与日表同口径：按 api 汇总
	apis, err := st.LoadDailyAPIs(ctx, "2026-03-01", "2026-03-31")
	if err != nil {
		t.Fatal(err)
	}
	var apiOK int64
	for _, a := range apis {
		if a.Key == "openai" {
			apiOK += a.Requests
		}
	}
	if apiOK != 4 {
		t.Fatalf("openai 协议应累计 4 次，实际 %d", apiOK)
	}
}

// 同一批流水重复累加会翻倍，这是 upsert 累加的语义：调用方保证不重放。
func TestFoldDailyUpserts(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	SetDailyLocation(time.UTC)
	ts := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	st.foldDaily(ctx, []UsageLog{{TS: ts, Model: "m", Status: 200, TotalTokens: 10}})
	st.foldDaily(ctx, []UsageLog{{TS: ts, Model: "m", Status: 200, TotalTokens: 20}})
	daily, _ := st.LoadDaily(ctx, "2026-04-01", "2026-04-01")
	if len(daily) != 1 || daily[0].Tokens != 30 || daily[0].Requests != 2 {
		t.Fatalf("两次写入应累加: %+v", daily)
	}
}

// 升级时从历史流水补算日聚合：表为空时执行，已有数据时跳过。
func TestBackfillDailyIfEmpty(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	SetDailyLocation(time.UTC)
	now := time.Now().UnixMilli()
	for _, u := range []UsageLog{
		{TS: now, Model: "a", Status: 200, TotalTokens: 5},
		{TS: now, Model: "b", Status: 200, TotalTokens: 7},
	} {
		if err := st.AddUsage(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.BackfillDailyIfEmpty(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应补算 2 条，实际 %d", n)
	}
	// 再跑一次不应重复累加
	if n2, _ := st.BackfillDailyIfEmpty(ctx); n2 != 0 {
		t.Fatalf("表已有数据时不应再补算，实际 %d", n2)
	}
}

func TestNormalizeReportRange(t *testing.T) {
	loc := time.UTC
	// 固定「今天」不可注入，这里只校验相对口径的宽度。
	r7 := NormalizeReportRange("7", "", "", "", loc)
	if r7.RangeDays() != 7 {
		t.Fatalf("7 天区间应为 7 天，实际 %d", r7.RangeDays())
	}
	r30 := NormalizeReportRange("30", "", "", "", loc)
	if r30.RangeDays() != 30 {
		t.Fatalf("30 天区间应为 30 天，实际 %d", r30.RangeDays())
	}
	// 倒置的自定义区间会被纠正
	rc := NormalizeReportRange("custom", "2026-05-10", "2026-05-01", "", loc)
	if rc.Start.After(rc.End) {
		t.Fatal("自定义区间应自动纠正倒置")
	}
	// 缺失日期时回退到近 7 天
	rbad := NormalizeReportRange("custom", "", "", "", loc)
	if rbad.RangeDays() != 7 {
		t.Fatalf("无日期应回退 7 天，实际 %d", rbad.RangeDays())
	}
}

func TestCurrentStreak(t *testing.T) {
	loc := time.UTC
	today := time.Date(2026, 6, 10, 15, 0, 0, 0, loc)
	byDay := map[string]int64{
		"2026-06-08": 3, "2026-06-09": 1, "2026-06-10": 2,
	}
	if got := CurrentStreak(byDay, today, loc); got != 3 {
		t.Fatalf("应为 3 天连续，实际 %d", got)
	}
	// 今天还没请求：从昨天往回数，不能把连续打断
	byDay["2026-06-10"] = 0
	if got := CurrentStreak(byDay, today, loc); got != 2 {
		t.Fatalf("今天无请求应数到 2 天，实际 %d", got)
	}
}

func TestPercentileIndex(t *testing.T) {
	if got := percentileIndex(100, 50); got != 49 {
		t.Fatalf("100 个样本的 P50 应取下标 49，实际 %d", got)
	}
	if got := percentileIndex(100, 95); got != 94 {
		t.Fatalf("100 个样本的 P95 应取下标 94，实际 %d", got)
	}
	if got := percentileIndex(1, 95); got != 0 {
		t.Fatalf("单样本应取下标 0，实际 %d", got)
	}
}
