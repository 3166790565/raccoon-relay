package store

import (
	"context"
	"testing"
	"time"
)

// 历史误记归正：502 + context canceled → 499，并重算当天的日聚合。
func TestMigrateLegacyCanceled(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	SetDailyLocation(time.UTC)

	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	// 两条被误记的客户端取消 + 一条真实 502 + 一条 200。
	_ = s.AddUsage(ctx, UsageLog{TS: ts, Model: "m", API: "openai", Status: 502,
		Error: "上游流读取中断: context canceled", MS: 5300})
	_ = s.AddUsage(ctx, UsageLog{TS: ts + 1, Model: "m", API: "openai", Status: 502,
		Error: "上游流读取中断: context canceled", MS: 6600})
	_ = s.AddUsage(ctx, UsageLog{TS: ts + 2, Model: "m", API: "openai", Status: 502,
		Error: "upstream boom", MS: 100})
	_ = s.AddUsage(ctx, UsageLog{TS: ts + 3, Model: "m", API: "openai", Status: 200,
		TotalTokens: 42, PromptTokens: 20, CompletionTokens: 22})

	// 模拟升级前的报表：旧版 foldDaily 不区分 499，4 条全部计入请求数。
	// 这里直接写聚合行，避免依赖当前版本的 foldDaily 口径（那正是被改掉的部分）。
	if _, err := s.Exec(`INSERT INTO request_daily
		(day, requests, successful, tokens, prompt_tokens, completion_tokens)
		VALUES ('2026-06-01', 4, 1, 42, 20, 22)`); err != nil {
		t.Fatal(err)
	}
	before, _ := s.LoadDaily(ctx, "2026-06-01", "2026-06-01")
	if len(before) != 1 || before[0].Requests != 4 || before[0].Successful != 1 {
		t.Fatalf("前置条件不成立：%+v", before)
	}

	n, err := s.MigrateLegacyCanceled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应归正 2 条，实际 %d", n)
	}

	// 流水：只有真实 502 还留着 502。
	rows, _, failed, err := s.QueryUsage(ctx, UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("归正后失败只该剩 1 条（真实 502），实际 %d", failed)
	}
	canceled := 0
	for _, r := range rows {
		if r.Status == UsageStatusCanceled {
			canceled++
		}
	}
	if canceled != 2 {
		t.Fatalf("应有 2 条 499，实际 %d", canceled)
	}

	// 日聚合重算：499 不进请求数，剩 2 条（502 + 200）。
	after, _ := s.LoadDaily(ctx, "2026-06-01", "2026-06-01")
	if len(after) != 1 {
		t.Fatalf("应仍有 1 天聚合，实际 %d", len(after))
	}
	if after[0].Requests != 2 || after[0].Successful != 1 || after[0].Tokens != 42 {
		t.Fatalf("重算后应为 2 条请求 / 1 成功 / 42 token，实际 %+v", after[0])
	}

	// 幂等：再跑一次什么都不改。
	again, err := s.MigrateLegacyCanceled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("第二次应跳过，实际改了 %d 条", again)
	}
}

// 真实上游 5xx 的错误文案里没有 context canceled，不能被误伤。
func TestMigrateLegacyCanceledLeavesRealErrors(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	SetDailyLocation(time.UTC)
	ts := time.Now().UnixMilli()
	_ = s.AddUsage(ctx, UsageLog{TS: ts, Status: 502, Error: "上游连接失败: dial tcp: i/o timeout"})
	_ = s.AddUsage(ctx, UsageLog{TS: ts, Status: 502,
		Error: "上游超过 120 秒无数据，已断开"}) // 我们的空闲超时，不是客户端取消

	n, err := s.MigrateLegacyCanceled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("不该改任何真实上游错误，实际改了 %d 条", n)
	}
	_, _, failed, _ := s.QueryUsage(ctx, UsageFilter{})
	if failed != 2 {
		t.Fatalf("真实 502 仍应计为失败，实际 %d", failed)
	}
}
