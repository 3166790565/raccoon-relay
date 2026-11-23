package admin

import (
	"testing"

	"github.com/3166790565/raccoon-relay/internal/config"
)

// 余额巡检设置：热更新、持久化、重启恢复。
func TestPutBalanceSyncSettingsHotUpdatesAndPersists(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	w := putSettings(t, p, `{"balanceSyncIntervalMin":20,"balanceSyncStaleMin":45}`)
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	if p.cfg.BalanceSyncIntervalMin != 20 || p.cfg.BalanceSyncStaleMin != 45 {
		t.Fatalf("热更新未生效: %d/%d", p.cfg.BalanceSyncIntervalMin, p.cfg.BalanceSyncStaleMin)
	}
	if raw := p.st.GetSetting(ctx, "balanceSyncIntervalMin", ""); raw != "20" {
		t.Fatalf("间隔未持久化: %q", raw)
	}
	if raw := p.st.GetSetting(ctx, "balanceSyncStaleMin", ""); raw != "45" {
		t.Fatalf("陈旧窗口未持久化: %q", raw)
	}

	// 重启：新的 Panel 从库里盖回
	p2 := &Panel{cfg: &config.Config{}, st: p.st}
	p2.applyStoredBalanceSyncSettings(ctx)
	if p2.cfg.BalanceSyncIntervalMin != 20 || p2.cfg.BalanceSyncStaleMin != 45 {
		t.Fatalf("重启后未恢复: %d/%d", p2.cfg.BalanceSyncIntervalMin, p2.cfg.BalanceSyncStaleMin)
	}

	// 关闭是合法值：0 必须存进去，而不是被当成未提交。
	w = putSettings(t, p, `{"balanceSyncIntervalMin":0}`)
	if w.Code != 200 {
		t.Fatalf("关闭应被接受: %d %s", w.Code, w.Body.String())
	}
	if raw := p.st.GetSetting(ctx, "balanceSyncIntervalMin", ""); raw != "0" {
		t.Fatalf("关闭应持久化为 0，实际 %q", raw)
	}
}

// 陈旧窗口小于巡检间隔会让每轮都重查全部账号，必须整体拒绝且不改现有值。
func TestPutBalanceSyncRejectsStaleBelowInterval(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	p.cfg.BalanceSyncIntervalMin = 10
	p.cfg.BalanceSyncStaleMin = 30

	w := putSettings(t, p, `{"balanceSyncIntervalMin":60,"balanceSyncStaleMin":15}`)
	if w.Code != 400 {
		t.Fatalf("陈旧窗口小于间隔应拒绝，实际 %d: %s", w.Code, w.Body.String())
	}
	if p.cfg.BalanceSyncIntervalMin != 10 || p.cfg.BalanceSyncStaleMin != 30 {
		t.Fatalf("拒绝时不应改动现有值: %d/%d", p.cfg.BalanceSyncIntervalMin, p.cfg.BalanceSyncStaleMin)
	}
	if raw := p.st.GetSetting(ctx, "balanceSyncIntervalMin", ""); raw != "" {
		t.Fatalf("拒绝时不应落库，实际 %q", raw)
	}

	// 越界同样拒绝
	for _, body := range []string{
		`{"balanceSyncIntervalMin":-1}`,
		`{"balanceSyncIntervalMin":1441}`,
		`{"balanceSyncStaleMin":10081}`,
	} {
		if w := putSettings(t, p, body); w.Code != 400 {
			t.Fatalf("%s 应拒绝，实际 %d", body, w.Code)
		}
	}
}

// 读取接口回填的是生效值，而不是环境变量默认值。
func TestGetSettingsIncludesBalanceSync(t *testing.T) {
	p, _, _ := newTestPanel(t)
	_ = putSettings(t, p, `{"balanceSyncIntervalMin":33,"balanceSyncStaleMin":66}`)
	s := getSettings(t, p)
	if got := s["balanceSyncIntervalMin"]; got != float64(33) && got != 33 {
		t.Fatalf("回填值不对: %v (%T)", got, got)
	}
	if got := s["balanceSyncStaleMin"]; got != float64(66) && got != 66 {
		t.Fatalf("回填值不对: %v (%T)", got, got)
	}
}
