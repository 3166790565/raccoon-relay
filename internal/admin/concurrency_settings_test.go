package admin

import (
	"context"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/cache"
	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// newConcurrencyPanel 构造一个带数据面服务的面板，用来验证并发设置的热更新。
func newConcurrencyPanel(t *testing.T) (*Panel, *relay.Server) {
	t.Helper()
	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{AdminToken: "adm", DataDir: t.TempDir(), MaxRetries: 3,
		PoolStrategy: "load_balance", MaxConcurrent: 0, QueueWaitSec: 600}
	srv := relay.NewServer(cfg, st, nil, nil)
	srv.SetSemaphore(cache.New("", "").NewSemaphore("relay"))
	p := &Panel{
		cfg:    cfg,
		st:     st,
		engine: relay.BuildEngine(cfg, st, nil, nil, nil, nil),
		po:     pool.New(st, "load_balance", pool.NewCooldownBook(st, true)),
		cool:   pool.NewCooldownBook(st, true),
		srv:    srv,
	}
	return p, srv
}

// 保存后应立即热更新到运行中的服务，并持久化到 settings。
func TestPutConcurrencyHotUpdatesAndPersists(t *testing.T) {
	p, srv := newConcurrencyPanel(t)
	ctx := context.Background()

	w := putSettings(t, p, `{"maxConcurrent":8,"queueWaitSec":120}`)
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	limit, wait := srv.ConcurrencyLimit()
	if limit != 8 || wait != 120 {
		t.Fatalf("热更新未生效: limit=%d wait=%d", limit, wait)
	}
	if raw := p.st.GetSetting(ctx, "maxConcurrent", ""); raw != "8" {
		t.Fatalf("maxConcurrent 未持久化，实际 %q", raw)
	}
	if raw := p.st.GetSetting(ctx, "queueWaitSec", ""); raw != "120" {
		t.Fatalf("queueWaitSec 未持久化，实际 %q", raw)
	}
	// GET 应返回当前生效值，前端据此回填。
	if got := getSettings(t, p)["maxConcurrent"]; got != float64(8) {
		t.Fatalf("GET 应返回 8，实际 %v", got)
	}
}

// 0 表示不限制，是合法值（用于关闭并发限制）。
func TestPutConcurrencyAcceptsZero(t *testing.T) {
	p, srv := newConcurrencyPanel(t)
	if w := putSettings(t, p, `{"maxConcurrent":8}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := putSettings(t, p, `{"maxConcurrent":0}`); w.Code != 200 {
		t.Fatalf("0 应被接受: %d %s", w.Code, w.Body.String())
	}
	if limit, _ := srv.ConcurrencyLimit(); limit != 0 {
		t.Fatalf("上限应回到 0（不限制），实际 %d", limit)
	}
}

// 非法值整体拒绝，不留下半套配置。
func TestPutConcurrencyRejectsInvalid(t *testing.T) {
	p, srv := newConcurrencyPanel(t)
	_ = putSettings(t, p, `{"maxConcurrent":8,"queueWaitSec":120}`)

	if w := putSettings(t, p, `{"maxConcurrent":-1}`); w.Code != 400 {
		t.Fatalf("负数应拒绝，实际 %d", w.Code)
	}
	if w := putSettings(t, p, `{"queueWaitSec":0}`); w.Code != 400 {
		t.Fatalf("排队时长 0 应拒绝，实际 %d", w.Code)
	}
	if w := putSettings(t, p, `{"queueWaitSec":9999}`); w.Code != 400 {
		t.Fatalf("超上限应拒绝，实际 %d", w.Code)
	}
	// 失败不应改动已生效的值。
	if limit, wait := srv.ConcurrencyLimit(); limit != 8 || wait != 120 {
		t.Fatalf("非法保存不应改动现有配置: limit=%d wait=%d", limit, wait)
	}
}

// 重启后面板保存的值要能盖回环境变量默认值。
func TestApplyStoredConcurrencySettings(t *testing.T) {
	p, srv := newConcurrencyPanel(t)
	ctx := context.Background()
	_ = p.st.SetSetting(ctx, "maxConcurrent", "16")
	_ = p.st.SetSetting(ctx, "queueWaitSec", "300")

	p.applyStoredConcurrencySettings(ctx)
	if limit, wait := srv.ConcurrencyLimit(); limit != 16 || wait != 300 {
		t.Fatalf("持久化值未恢复: limit=%d wait=%d", limit, wait)
	}
}
