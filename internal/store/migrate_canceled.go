package store

import (
	"context"
	"fmt"
	"time"
)

// dayKey 把一个毫秒时间戳压成日聚合用的自然日键（与 foldDaily 同一时区）。
func dayKey(ts int64, loc *time.Location) string {
	return time.UnixMilli(ts).In(loc).Format("2006-01-02")
}

// dayBounds 返回某个自然日的 [起, 止) 毫秒区间。
func dayBounds(day string, loc *time.Location) (start, end int64, err error) {
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		return 0, 0, fmt.Errorf("解析自然日 %q: %w", day, err)
	}
	return t.UnixMilli(), t.AddDate(0, 0, 1).UnixMilli(), nil
}

// legacyCanceledMigratedKey 标记「历史 502 context canceled → 499」这一步是否跑过。
//
// 为什么需要它：旧版本把客户端中途断开（用户点停止、关掉页面、客户端自己超时）
// 包成 UpstreamError 时丢掉了 context.Canceled，转发层认不出来，于是流水记成
// 502「上游流读取中断: context canceled」。这些行既不是上游故障，也不该进失败数
// 和成功率——它们只是请求方提前走了。
//
// 代码已修，但历史流水还在库里：不归正的话，面板上那几个 502 会一直挂到今天这个
// 统计窗口里，用户仍然会把它们当成真的上游错误。
const legacyCanceledMigratedKey = "migrate:legacy-canceled-499"

// statusCanceled 与 UsageStatusCanceled 同值。这里单独起名，是为了让这段一次性
// 迁移的意图明确：501 不是「本版本的语义」，而是「把历史误记归正到的目标值」。
const statusCanceled = UsageStatusCanceled

// MigrateLegacyCanceled 把历史里被误记成 502 的「客户端取消」改成 499，
// 并从流水重算受影响的日聚合。幂等：靠 settings 里的一行标记只跑一次。
//
// 只认「状态 502 且错误文案含 context canceled」这一种组合：
//   - 上游 5xx 的原文不会带这个串（它是 Go 的 context 包文案）；
//   - 我们自己空闲超时的文案是「上游超过 N 秒无数据」，不含 context canceled。
//
// 所以这个判据不会误伤真实的上游故障。
func (s *Store) MigrateLegacyCanceled(ctx context.Context) (int64, error) {
	if _, done := s.GetSettingOK(ctx, legacyCanceledMigratedKey); done {
		return 0, nil
	}

	// 先找出受影响的日期，改完状态后按天重算聚合。
	days, err := s.canceledLegacyDays(ctx)
	if err != nil {
		return 0, err
	}

	res, err := s.exec(ctx,
		`UPDATE usage_logs SET status = ?
		 WHERE status = 502 AND error LIKE '%context canceled%'`, statusCanceled)
	if err != nil {
		return 0, fmt.Errorf("归正历史客户端取消流水: %w", err)
	}
	n, _ := res.RowsAffected()

	if n > 0 {
		if err := s.rebuildDailyForDays(ctx, days); err != nil {
			// 聚合重算失败不回滚流水归正：流水是账，聚合是报表缓存。
			// 下次启动还会重试（标记尚未写入），但流水本身已经正确了。
			return n, err
		}
	}
	if err := s.SetSetting(ctx, legacyCanceledMigratedKey, "1"); err != nil {
		return n, err
	}
	return n, nil
}

// canceledLegacyDays 返回待归正的流水落在哪些自然日（按日聚合的时区）。
func (s *Store) canceledLegacyDays(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx,
		`SELECT DISTINCT ts FROM usage_logs
		 WHERE status = 502 AND error LIKE '%context canceled%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	loc := dailyLocation()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			return nil, err
		}
		day := dayKey(ts, loc)
		if !seen[day] {
			seen[day] = true
			out = append(out, day)
		}
	}
	return out, rows.Err()
}

// rebuildDailyForDays 按 usage_logs 重新计算指定日期的三张日聚合表。
//
// 直接用「按天重算」而不是「从聚合里减掉」，是因为后者要还原成功数/请求数/三类
// token 六个计数，任何一处算错都会让报表永久偏；重算一遍代价只有这几天。
func (s *Store) rebuildDailyForDays(ctx context.Context, days []string) error {
	if len(days) == 0 {
		return nil
	}
	loc := dailyLocation()
	for _, day := range days {
		start, end, err := dayBounds(day, loc)
		if err != nil {
			return err
		}
		rows, err := s.query(ctx,
			`SELECT ts, account_id, model, api, status, prompt_tokens, completion_tokens, total_tokens,
				cached_tokens
			 FROM usage_logs WHERE ts >= ? AND ts < ?`, start, end)
		if err != nil {
			return err
		}
		var batch []UsageLog
		for rows.Next() {
			var u UsageLog
			if err := rows.Scan(&u.TS, &u.AccountID, &u.Model, &u.API, &u.Status,
				&u.PromptTokens, &u.CompletionTokens, &u.TotalTokens, &u.CachedTokens); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, u)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}

		// 先清空这一天，再整批重算。三张表要一起清，否则分解表会累加到旧值上。
		for _, table := range []string{"request_daily", "request_daily_model", "request_daily_api"} {
			if _, err := s.exec(ctx, `DELETE FROM `+table+` WHERE day = ?`, day); err != nil {
				return err
			}
		}
		if len(batch) == 0 {
			continue
		}
		s.foldDaily(ctx, batch)
	}
	return nil
}
