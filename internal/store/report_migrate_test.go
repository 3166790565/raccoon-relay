package store

import (
	"context"
	"testing"
	"time"
)

// 上一版的报表聚合表只有 day/requests/successful/tokens 四列。
// 升级后必须自动补出 prompt_tokens/completion_tokens（以及分解表的 successful），
// 否则新聚合 SQL 会报「no such column」，报表页直接 500。
func TestReportColumnsBackfilledOnUpgrade(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// 模拟旧库：删掉新列所在的表，再按旧结构重建。
	for _, tbl := range []string{"request_daily", "request_daily_model", "request_daily_api"} {
		if _, err := st.Exec(`DROP TABLE IF EXISTS ` + tbl); err != nil {
			t.Fatal(err)
		}
	}
	for _, ddl := range []string{
		`CREATE TABLE request_daily (day TEXT PRIMARY KEY, requests INTEGER NOT NULL DEFAULT 0,
			successful INTEGER NOT NULL DEFAULT 0, tokens INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE request_daily_model (day TEXT NOT NULL, model TEXT NOT NULL DEFAULT '',
			requests INTEGER NOT NULL DEFAULT 0, tokens INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (day, model))`,
	} {
		if _, err := st.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	// 模拟真实升级：走一遍完整 migrate（schema 建出 request_daily_api、补列逻辑补老表）。
	if err := st.migrate(); err != nil {
		t.Fatalf("升级 migrate 失败: %v", err)
	}
	for _, p := range [][2]string{
		{"request_daily", "prompt_tokens"},
		{"request_daily", "completion_tokens"},
		{"request_daily_model", "successful"},
		{"request_daily_model", "prompt_tokens"},
		{"request_daily_model", "completion_tokens"},
	} {
		ok, err := columnExists(st.DB(), p[0], p[1])
		if err != nil || !ok {
			t.Fatalf("%s.%s 应已补齐（err=%v ok=%v）", p[0], p[1], err, ok)
		}
	}

	// 补列后聚合与读取都要能正常工作。
	SetDailyLocation(time.UTC)
	ts := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC).UnixMilli()
	st.foldDaily(ctx, []UsageLog{{TS: ts, Model: "m", API: "openai", Status: 200,
		TotalTokens: 30, PromptTokens: 10, CompletionTokens: 20}})
	daily, err := st.LoadDaily(ctx, "2026-07-01", "2026-07-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 1 || daily[0].Completion != 20 {
		t.Fatalf("补列后聚合应可用: %+v", daily)
	}
}
