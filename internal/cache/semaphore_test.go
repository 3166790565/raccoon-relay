package cache

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 并发上限生效：同时运行的不会超过 limit。
func TestSemaphoreLimitsConcurrency(t *testing.T) {
	for _, tc := range []struct {
		name string
		sem  Semaphore
	}{
		{"local", New("", "").NewSemaphore("relay")},
		{"redis", redisSemIfAvailable(t)},
	} {
		tc := tc
		if tc.sem == nil {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			const limit = 3
			var running, peak int64
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					release, err := tc.sem.Acquire(context.Background(), limit, time.Minute)
					if err != nil {
						t.Errorf("Acquire 失败: %v", err)
						return
					}
					n := atomic.AddInt64(&running, 1)
					for {
						p := atomic.LoadInt64(&peak)
						if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
							break
						}
					}
					time.Sleep(30 * time.Millisecond)
					atomic.AddInt64(&running, -1)
					release()
				}()
			}
			wg.Wait()
			if peak > limit {
				t.Fatalf("峰值并发 %d 超过上限 %d", peak, limit)
			}
		})
	}
}

// 取消排队的请求应立即返回，而不是一直等。
func TestSemaphoreQueueRespectsContext(t *testing.T) {
	sem := New("", "").NewSemaphore("relay")
	release, err := sem.Acquire(context.Background(), 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := sem.Acquire(ctx, 1, time.Minute); err == nil {
		t.Fatal("槽位已满且 ctx 超时，应返回错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("应在 ctx 取消后立即返回，实际等了 %v", elapsed)
	}
}

// 释放后等待者应能继续。
func TestSemaphoreReleaseWakesWaiter(t *testing.T) {
	sem := New("", "").NewSemaphore("relay")
	release, err := sem.Acquire(context.Background(), 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		r, err := sem.Acquire(context.Background(), 1, time.Minute)
		if err == nil {
			r()
		}
		done <- err
	}()

	select {
	case <-done:
		t.Fatal("槽位未释放前不应获得")
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("释放后应获得槽位: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("释放后等待者没有被唤醒")
	}
}

// Redis 模式下过期的槽位应被自动回收（进程崩溃没释放）。
func TestRedisSemaphoreReclaimsExpiredSlots(t *testing.T) {
	sem := redisSemIfAvailable(t)
	if sem == nil {
		t.Skip("未设置 TEST_REDIS_URL")
	}
	// TTL 100ms：拿住不放，等它过期后另一个实例仍应能拿到。
	if _, err := sem.Acquire(context.Background(), 1, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := sem.Acquire(ctx, 1, time.Minute)
	if err != nil {
		t.Fatalf("过期槽位应被回收: %v", err)
	}
	release()
}

func redisSemIfAvailable(t *testing.T) Semaphore {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		return nil
	}
	c := New(url, "test-sem:")
	c.Start(context.Background(), false)
	if !c.Enabled() {
		t.Skip("Redis 不可用")
	}
	return c.NewSemaphore("relay")
}
