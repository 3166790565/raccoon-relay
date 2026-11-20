package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/store"
)

func newTestPanel(t *testing.T) (*Panel, *relay.Renewer, context.Context) {
	t.Helper()
	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rw := relay.NewRenewer(nil, relay.RenewConfig{
		Window: 72 * time.Hour, Interval: 6 * time.Hour, Gap: time.Second,
	})
	cfg := &config.Config{AdminToken: "adm", DataDir: t.TempDir(), MaxRetries: 3, PoolStrategy: "optimal"}
	// handleGetSettings / handlePutSettings 会读 engine 与 pool，
	// 这里给最小可用的空引擎，避免测试里出现 nil 解引用。
	p := &Panel{
		cfg:     cfg,
		st:      st,
		renewer: rw,
		engine:  relay.BuildEngine(cfg, st, nil, nil, nil, nil),
		po:      pool.New(st, "optimal", pool.NewCooldownBook(st, true)),
		cool:    pool.NewCooldownBook(st, true),
	}
	return p, rw, context.Background()
}

func putSettings(t *testing.T, p *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.handlePutSettings(w, req)
	return w
}

// 保存后续期参数要立刻热更新到 Renewer，并且落库。
func TestPutRenewSettingsHotUpdates(t *testing.T) {
	p, rw, ctx := newTestPanel(t)
	w := putSettings(t, p, `{"renewWindowSec":604800,"renewIntervalMin":120,"renewGapMS":500}`)
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	cfg := rw.Config()
	if cfg.Window != 7*24*time.Hour || cfg.Interval != 2*time.Hour || cfg.Gap != 500*time.Millisecond {
		t.Fatalf("热更新未生效: %+v", cfg)
	}
	if raw := p.st.GetSetting(ctx, "renewWindowSec", ""); raw != "604800" {
		t.Fatalf("未持久化: %q", raw)
	}
	// 重启后 applyStoredRenewSettings 应把值盖回去
	rw2 := relay.NewRenewer(nil, relay.RenewConfig{Window: time.Hour, Interval: time.Minute})
	p2 := &Panel{cfg: p.cfg, st: p.st, renewer: rw2}
	p2.applyStoredRenewSettings(ctx)
	if got := rw2.Config(); got.Window != 7*24*time.Hour || got.Interval != 2*time.Hour {
		t.Fatalf("重启后未恢复存储值: %+v", got)
	}
}

// 扫描间隔大于提前量会让号在两次扫描之间过期，必须拒绝。
func TestPutRenewSettingsRejectsIntervalOverWindow(t *testing.T) {
	p, rw, _ := newTestPanel(t)
	before := rw.Config()
	w := putSettings(t, p, `{"renewWindowSec":3600,"renewIntervalMin":120}`)
	if w.Code != 400 {
		t.Fatalf("应拒绝非法组合，实际 %d", w.Code)
	}
	if rw.Config() != before {
		t.Fatal("校验失败时不该改动运行期配置")
	}
}

// 0 表示关闭，是合法值。
func TestPutRenewSettingsAllowsDisable(t *testing.T) {
	p, rw, _ := newTestPanel(t)
	w := putSettings(t, p, `{"renewWindowSec":0}`)
	if w.Code != 200 {
		t.Fatalf("0 应表示关闭并允许保存: %d %s", w.Code, w.Body.String())
	}
	if rw.Config().Window != 0 {
		t.Fatal("窗口未清零")
	}
	if st := rw.Status(); st["enabled"] != false {
		t.Fatalf("状态应显示已关闭: %+v", st)
	}
}

// GET /api/settings 要带上续期字段，供面板回填。
func TestGetSettingsIncludesRenew(t *testing.T) {
	p, _, _ := newTestPanel(t)
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Header.Set("Authorization", "Bearer adm")
	w := httptest.NewRecorder()
	p.handleGetSettings(w, req)
	var resp struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Settings["renewWindowSec"] != float64(259200) {
		t.Fatalf("缺少或错误的 renewWindowSec: %v", resp.Settings["renewWindowSec"])
	}
}
