package store

import (
	"context"
	"fmt"
	"log"
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

// insertUsageBatch 用一条 multi-row INSERT 写入一批流水。
func (s *Store) insertUsageBatch(ctx context.Context, batch []UsageLog) error {
	ph := make([]string, 0, len(batch))
	args := make([]any, 0, len(batch)*10)
	for _, u := range batch {
		if u.TS == 0 {
			u.TS = time.Now().UnixMilli()
		}
		ph = append(ph, "(?,?,?,?,?,?,?,?,?,?)")
		args = append(args, u.TS, u.AccountID, u.Model, u.API, u.Status,
			u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.MS, u.Error)
	}
	q := `INSERT INTO usage_logs (ts, account_id, model, api, status,
		prompt_tokens, completion_tokens, total_tokens, ms, error) VALUES ` + joinStrings(ph, ",")
	_, err := s.exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("insert usage batch: %w", err)
	}
	return nil
}