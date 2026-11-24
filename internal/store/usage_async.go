package store

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// 调用流水异步写入。
//
// 每个转发请求原本都在处理 goroutine 里同步 INSERT 一条 usage_logs，
// 在单连接 SQLite 上这意味着请求要排队等写锁。改成后台批量写：
// 请求只把记录塞进带缓冲的 channel，真正的 INSERT 每 500ms 或每 100 条做一次。
//
// 必须用独立的 context.Background()：请求 ctx 在 handler 返回后就被取消，
// 沿用它会让所有异步写失败。
const (
	usageQueueSize = 4096
	usageBatchMax  = 100
	usageFlushGap  = 500 * time.Millisecond
)

// AddUsageAsync 把一条流水交给后台写入器。队列满时退化为同步写，
// 宁可慢也不能丢账。
func (s *Store) AddUsageAsync(u UsageLog) {
	s.startUsageWriter()
	atomic.AddInt64(&s.usagePending, 1)
	select {
	case s.usageQ <- u:
	default:
		// 队列满：同步兜底（不计入 pending，下面直接扣掉）。
		_ = s.AddUsage(context.Background(), u)
		atomic.AddInt64(&s.usagePending, -1)
	}
}

// FlushUsage 等待已入队的记录全部落库，用于优雅关闭。
// 不能只看 channel 长度：记录可能已经进了批次但还没写。
func (s *Store) FlushUsage() {
	if s.usageQ == nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&s.usagePending) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// StopUsage 结束异步写手并等待它把最后一批刷完。
// 调用后不应再有 AddUsageAsync。
func (s *Store) StopUsage() {
	if s.usageQ == nil {
		return
	}
	s.usageStopOnce.Do(func() { close(s.usageQ) })
	// 写手收到 channel 关闭后会 flush 并退出，等 pending 归零即可。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&s.usagePending) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Store) startUsageWriter() {
	s.usageOnce.Do(func() {
		s.usageQ = make(chan UsageLog, usageQueueSize)
		go s.usageLoop()
	})
}

func (s *Store) usageLoop() {
	ticker := time.NewTicker(usageFlushGap)
	defer ticker.Stop()
	batch := make([]UsageLog, 0, usageBatchMax)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		n := int64(len(batch))
		if err := s.insertUsageBatch(context.Background(), batch); err != nil {
			log.Printf("[流水] 批量写入失败，丢弃 %d 条: %v", len(batch), err)
		}
		atomic.AddInt64(&s.usagePending, -n)
		batch = batch[:0]
	}
	for {
		select {
		case u, ok := <-s.usageQ:
			if !ok {
				flush()
				return
			}
			batch = append(batch, u)
			if len(batch) >= usageBatchMax {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// insertUsageBatch 用一条 multi-row INSERT 写入一批流水，并同步累加日聚合。
func (s *Store) insertUsageBatch(ctx context.Context, batch []UsageLog) error {
	ph := make([]string, 0, len(batch))
	args := make([]any, 0, len(batch)*11)
	for _, u := range batch {
		if u.TS == 0 {
			u.TS = time.Now().UnixMilli()
		}
		ph = append(ph, "(?,?,?,?,?,?,?,?,?,?,?)")
		args = append(args, u.TS, u.AccountID, u.Model, u.API, u.Status,
			u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.CachedTokens, u.MS, u.Error)
	}
	q := `INSERT INTO usage_logs (ts, account_id, model, api, status,
		prompt_tokens, completion_tokens, total_tokens, cached_tokens, ms, error) VALUES ` + joinStrings(ph, ",")
	if _, err := s.exec(ctx, q, args...); err != nil {
		return fmt.Errorf("insert usage batch: %w", err)
	}
	s.foldDaily(ctx, batch)
	return nil
}

// foldDaily 把一批流水累加进 request_daily / request_daily_model。
//
// 报表只读这两张小表：一年热力图、长区间趋势都不必扫 usage_logs 大表。
// 聚合失败只记日志、不回滚流水——流水是账，聚合只是报表缓存，不能让它挡住记账。
func (s *Store) foldDaily(ctx context.Context, batch []UsageLog) {
	if len(batch) == 0 {
		return
	}
	loc := dailyLocation()
	type agg struct{ req, ok, tok, prompt, completion, cached int64 }
	add := func(a *agg, u UsageLog) {
		a.req++
		if u.Status >= 200 && u.Status < 400 {
			a.ok++
		}
		a.tok += u.TotalTokens
		a.prompt += u.PromptTokens
		a.completion += u.CompletionTokens
		a.cached += u.CachedTokens
	}
	days := map[string]*agg{}
	type mkey struct{ day, model string }
	models := map[mkey]*agg{}
	type akey struct{ day, api string }
	apis := map[akey]*agg{}
	for _, u := range batch {
		// 客户端取消（499）不计入任何日聚合：请求方自己走了，既不是这个号
		// 处理的请求，也不代表网关成功率。留在 requests 里会让报表的成功率
		// 永久偏低，且与账号/协议明细（已排除 499）对不上。
		if u.Status == UsageStatusCanceled {
			continue
		}
		ts := u.TS
		if ts == 0 {
			ts = time.Now().UnixMilli()
		}
		day := time.UnixMilli(ts).In(loc).Format("2006-01-02")
		d := days[day]
		if d == nil {
			d = &agg{}
			days[day] = d
		}
		add(d, u)
		mk := mkey{day, u.Model}
		m := models[mk]
		if m == nil {
			m = &agg{}
			models[mk] = m
		}
		add(m, u)
		ak := akey{day, u.API}
		a := apis[ak]
		if a == nil {
			a = &agg{}
			apis[ak] = a
		}
		add(a, u)
	}
	const dayQuery = `INSERT INTO request_daily
			(day, requests, successful, tokens, prompt_tokens, completion_tokens, cached_tokens) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(day) DO UPDATE SET
			requests = request_daily.requests + excluded.requests,
			successful = request_daily.successful + excluded.successful,
			tokens = request_daily.tokens + excluded.tokens,
			prompt_tokens = request_daily.prompt_tokens + excluded.prompt_tokens,
			completion_tokens = request_daily.completion_tokens + excluded.completion_tokens,
			cached_tokens = request_daily.cached_tokens + excluded.cached_tokens`
	for day, a := range days {
		if _, err := s.exec(ctx, dayQuery, day, a.req, a.ok, a.tok, a.prompt, a.completion, a.cached); err != nil {
			log.Printf("[报表] 累加日统计失败（%s）: %v", day, err)
			return
		}
	}
	const modelQuery = `INSERT INTO request_daily_model
			(day, model, requests, successful, tokens, prompt_tokens, completion_tokens, cached_tokens) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(day, model) DO UPDATE SET
			requests = request_daily_model.requests + excluded.requests,
			successful = request_daily_model.successful + excluded.successful,
			tokens = request_daily_model.tokens + excluded.tokens,
			prompt_tokens = request_daily_model.prompt_tokens + excluded.prompt_tokens,
			completion_tokens = request_daily_model.completion_tokens + excluded.completion_tokens,
			cached_tokens = request_daily_model.cached_tokens + excluded.cached_tokens`
	for k, a := range models {
		if _, err := s.exec(ctx, modelQuery, k.day, k.model, a.req, a.ok, a.tok, a.prompt, a.completion, a.cached); err != nil {
			log.Printf("[报表] 累加模型统计失败（%s/%s）: %v", k.day, k.model, err)
			return
		}
	}
	const apiQuery = `INSERT INTO request_daily_api
			(day, api, requests, successful, tokens, prompt_tokens, completion_tokens) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(day, api) DO UPDATE SET
			requests = request_daily_api.requests + excluded.requests,
			successful = request_daily_api.successful + excluded.successful,
			tokens = request_daily_api.tokens + excluded.tokens,
			prompt_tokens = request_daily_api.prompt_tokens + excluded.prompt_tokens,
			completion_tokens = request_daily_api.completion_tokens + excluded.completion_tokens`
	for k, a := range apis {
		if _, err := s.exec(ctx, apiQuery, k.day, k.api, a.req, a.ok, a.tok, a.prompt, a.completion); err != nil {
			log.Printf("[报表] 累加协议统计失败（%s/%s）: %v", k.day, k.api, err)
			return
		}
	}
}

// dailyLocation 是日聚合使用的时区。报表按自然日分组，必须与用户看到的
// 日期一致；没有显式设置时按本地时区（容器里通常由 TZ 决定）。
var (
	dailyLocMu sync.RWMutex
	dailyLoc   = time.Local
)

// SetDailyLocation 供装配层按 TZ 配置注入时区。
func SetDailyLocation(loc *time.Location) {
	if loc == nil {
		return
	}
	dailyLocMu.Lock()
	dailyLoc = loc
	dailyLocMu.Unlock()
}

func dailyLocation() *time.Location {
	dailyLocMu.RLock()
	defer dailyLocMu.RUnlock()
	return dailyLoc
}
