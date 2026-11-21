package admin

import (
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/pool"
)

// 保存后号池与引擎都要热更新，并持久化。
func TestPutDispatchSettingsHotUpdates(t *testing.T) {
	p, srv := newConcurrencyPanel(t)
	_ = srv
	w := putSettings(t, p, `{"leaseEnabled":true,"leaseTTLSec":120,"retryDelayMS":300,`+
		`"backoffEnabled":true,"backoffBaseMS":1500,"backoffMaxMS":20000,`+
		`"affinityEnabled":true,"affinityTTLSec":3600}`)
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}

	lease := p.po.LeaseConfig()
	if !lease.Enabled || lease.TTL != 120*time.Second {
		t.Fatalf("租约未热更新: %+v", lease)
	}
	bo := p.po.BackoffPolicy()
	if !bo.Enabled || bo.Base != 1500*time.Millisecond || bo.Max != 20*time.Second {
		t.Fatalf("退避未热更新: %+v", bo)
	}
	if got := p.engine.RetryDelay(); got != 300*time.Millisecond {
		t.Fatalf("换号间隔未热更新，实际 %s", got)
	}
	on, ttl := p.engine.AffinityPolicy()
	if !on || ttl != 3600*time.Second {
		t.Fatalf("会话粘性未热更新: on=%v ttl=%s", on, ttl)
	}
	if raw := p.st.GetSetting(t.Context(), "leaseTTLSec", ""); raw != "120" {
		t.Fatalf("leaseTTLSec 未持久化，实际 %q", raw)
	}
}

// 关闭开关是合法值，必须能保存成 false 而不是被当成「未提交」。
func TestPutDispatchSettingsCanDisable(t *testing.T) {
	p, _ := newConcurrencyPanel(t)
	_ = putSettings(t, p, `{"leaseEnabled":true,"backoffEnabled":true,"affinityEnabled":true}`)

	w := putSettings(t, p, `{"leaseEnabled":false,"backoffEnabled":false,"affinityEnabled":false}`)
	if w.Code != 200 {
		t.Fatalf("关闭应被接受: %d %s", w.Code, w.Body.String())
	}
	if p.po.LeaseConfig().Enabled {
		t.Fatal("租约开关未关闭")
	}
	if p.po.BackoffPolicy().Enabled {
		t.Fatal("退避开关未关闭")
	}
	if on, _ := p.engine.AffinityPolicy(); on {
		t.Fatal("会话粘性开关未关闭")
	}
}

// 非法值整体拒绝，且不改动已生效的配置。
func TestPutDispatchSettingsRejectsInvalid(t *testing.T) {
	p, _ := newConcurrencyPanel(t)
	_ = putSettings(t, p, `{"leaseEnabled":true,"leaseTTLSec":120,"retryDelayMS":300}`)

	cases := []string{
		`{"leaseTTLSec":10}`,                         // 低于下限
		`{"retryDelayMS":-1}`,                        // 负数
		`{"retryDelayMS":99999}`,                     // 超过上限
		`{"backoffBaseMS":50}`,                       // 起点过低
		`{"backoffBaseMS":5000,"backoffMaxMS":1000}`, // 上限小于起点
		`{"affinityTTLSec":10}`,                      // 绑定时长过短
	}
	for _, body := range cases {
		if w := putSettings(t, p, body); w.Code != 400 {
			t.Fatalf("%s 应被拒绝，实际 %d %s", body, w.Code, w.Body.String())
		}
	}
	if p.po.LeaseConfig().TTL != 120*time.Second {
		t.Fatalf("非法保存不应改动现有配置，实际 %s", p.po.LeaseConfig().TTL)
	}
	if p.engine.RetryDelay() != 300*time.Millisecond {
		t.Fatalf("非法保存不应改动换号间隔，实际 %s", p.engine.RetryDelay())
	}
}

// 重启后持久化值要能盖回运行中的号池与引擎。
func TestApplyStoredDispatchSettings(t *testing.T) {
	p, _ := newConcurrencyPanel(t)
	ctx := t.Context()
	_ = p.st.SetSetting(ctx, "leaseEnabled", "true")
	_ = p.st.SetSetting(ctx, "leaseTTLSec", "240")
	_ = p.st.SetSetting(ctx, "retryDelayMS", "750")
	_ = p.st.SetSetting(ctx, "backoffEnabled", "false")
	_ = p.st.SetSetting(ctx, "backoffBaseMS", "3000")
	_ = p.st.SetSetting(ctx, "affinityEnabled", "false")
	_ = p.st.SetSetting(ctx, "affinityTTLSec", "7200")

	p.applyStoredDispatchSettings(ctx)

	if l := p.po.LeaseConfig(); !l.Enabled || l.TTL != 240*time.Second {
		t.Fatalf("租约未恢复: %+v", l)
	}
	if p.engine.RetryDelay() != 750*time.Millisecond {
		t.Fatalf("换号间隔未恢复，实际 %s", p.engine.RetryDelay())
	}
	if b := p.po.BackoffPolicy(); b.Enabled || b.Base != 3*time.Second {
		t.Fatalf("退避未恢复: %+v", b)
	}
	if on, ttl := p.engine.AffinityPolicy(); on || ttl != 2*time.Hour {
		t.Fatalf("会话粘性未恢复: on=%v ttl=%s", on, ttl)
	}
}

// GET 返回当前生效值，前端据此回填。
func TestGetDispatchSettings(t *testing.T) {
	p, _ := newConcurrencyPanel(t)
	p.po.SetLeaseConfig(pool.LeaseConfig{Enabled: true, TTL: 90 * time.Second})
	p.engine.SetRetryDelay(1200 * time.Millisecond)
	p.engine.SetAffinityPolicy(true, time.Hour)

	got := getSettings(t, p)
	if got["leaseEnabled"] != true || got["leaseTTLSec"] != float64(90) {
		t.Fatalf("GET 租约字段不对: %v", got)
	}
	if got["retryDelayMS"] != float64(1200) {
		t.Fatalf("GET 换号间隔不对: %v", got["retryDelayMS"])
	}
	if got["affinityEnabled"] != true || got["affinityTTLSec"] != float64(3600) {
		t.Fatalf("GET 会话粘性字段不对: %v", got)
	}
}
