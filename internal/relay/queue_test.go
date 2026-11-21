package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/cache"
	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// 并发上限为 1 时，第二个请求应在排队而不是立即失败；
// 第一个请求结束后它应正常完成。
func TestQueueWaitsInsteadOfRejecting(t *testing.T) {
	var inflight, peak int64
	gate := make(chan struct{})

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&inflight, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
				break
			}
		}
		// 第一个请求卡住，给第二个制造排队窗口。
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
		}
		atomic.AddInt64(&inflight, -1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	st, err := store.Open(t.TempDir(), "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		MaxRetries: 3, MaxBodyMB: 32, DataDir: t.TempDir(),
		PoolStrategy: "load_balance", ModelCooldown: true, RefreshWindowSec: 300,
		MaxConcurrent: 1, QueueWaitSec: 30,
	}
	client := raccoon.NewClient(up.URL, 10*time.Second)
	cool := pool.NewCooldownBook(st, true)
	po := pool.New(st, "load_balance", cool)
	refresher := NewRefresher(st, client, 5*time.Minute)
	engine := BuildEngine(cfg, st, po, cool, client, refresher)
	srv := NewServer(cfg, st, engine, NewCatalog(st, client, DefaultModel))
	srv.SetSemaphore(cache.New("", "").NewSemaphore("relay"))

	acc := &store.Account{
		UID: "u1", Name: "u1", Token: testToken("u1"), RefreshToken: "rf",
		Status: store.StatusOnline, Credits: 100, CreditsAt: time.Now().UnixMilli(),
		TokenExp: time.Now().Add(2 * time.Hour).UnixMilli(), Enabled: true,
	}
	if err := st.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	_, key, err := st.CreateKey(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"` + DefaultModel + `","messages":[{"role":"user","content":"hi"}]}`
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Route(w, req)
		return w
	}

	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = call()
		}(i)
	}

	// 等第一个请求真正打到上游，再放行。
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&inflight) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt64(&inflight) == 0 {
		t.Fatal("第一个请求没有打到上游")
	}
	// 此刻第二个请求应该还卡在排队，没有进入上游。
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt64(&inflight); got != 1 {
		t.Fatalf("排队期间不应有第二个请求打到上游，实际在途 %d", got)
	}
	close(gate)
	wg.Wait()

	for i, w := range results {
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 个请求应成功，实际 %d: %s", i+1, w.Code, w.Body.String())
		}
	}
	if p := atomic.LoadInt64(&peak); p > 1 {
		t.Fatalf("并发上限 1，但上游观察到峰值 %d", p)
	}
}

// 排队超时应返回 503，而不是 429 或空响应。
func TestQueueTimeoutReturns503(t *testing.T) {
	block := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer up.Close()
	defer close(block)

	st, err := store.Open(t.TempDir(), "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		MaxRetries: 3, MaxBodyMB: 32, DataDir: t.TempDir(),
		PoolStrategy: "load_balance", ModelCooldown: true, RefreshWindowSec: 300,
		MaxConcurrent: 1, QueueWaitSec: 1,
	}
	client := raccoon.NewClient(up.URL, 10*time.Second)
	cool := pool.NewCooldownBook(st, true)
	po := pool.New(st, "load_balance", cool)
	refresher := NewRefresher(st, client, 5*time.Minute)
	engine := BuildEngine(cfg, st, po, cool, client, refresher)
	srv := NewServer(cfg, st, engine, NewCatalog(st, client, DefaultModel))
	sem := cache.New("", "").NewSemaphore("relay")
	srv.SetSemaphore(sem)

	// 手工占满唯一槽位。
	release, err := sem.Acquire(context.Background(), 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// 客户端一直在线，只是排队时间（QueueWaitSec=1s）到了。
	start := time.Now()
	_, err = srv.acquireSlot(context.Background(), "openai", DefaultModel)
	if err == nil {
		t.Fatal("槽位占满，等待超时后应返回错误")
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("应在排队 1s 后才超时，实际 %v", elapsed)
	}
	code, _ := publicError(err)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("排队超时应返回 503，实际 %d", code)
	}
}
