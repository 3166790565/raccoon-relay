package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/scheduler"
	"github.com/3166790565/raccoon-relay/internal/updater"
)

// 面板改签到设置必须落库，重启后由 applyStoredCheckinSettings 恢复——
// 只改内存会让「保存成功」在重启后悄悄失效。
func TestCheckinSettingsPersistAndRestore(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	p.sched = scheduler.New(p.cfg, p.st, nil, nil, nil)

	w := putSettings(t, p, `{"checkinEnable":false,"checkinHour":13,"checkinMinute":5,"checkinJitterMinutes":20}`)
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	if p.cfg.CheckinHour != 13 || p.cfg.CheckinMinute != 5 || p.cfg.CheckinJitterMin != 20 {
		t.Fatalf("内存未更新: %+v", p.cfg)
	}
	if p.cfg.CheckinEnable {
		t.Fatal("签到开关未关闭")
	}
	for key, want := range map[string]string{
		"checkinEnable": "false", "checkinHour": "13",
		"checkinMinute": "5", "checkinJitterMinutes": "20",
	} {
		if got := p.st.GetSetting(ctx, key, ""); got != want {
			t.Fatalf("%s 未持久化：得到 %q，期望 %q", key, got, want)
		}
	}

	// 模拟重启：新的 cfg 用环境默认值，再走 ApplyStoredSettings
	p2, _, _ := newTestPanel(t)
	p2.st = p.st
	p2.sched = scheduler.New(p2.cfg, p.st, nil, nil, nil)
	p2.cfg.CheckinEnable = true
	p2.cfg.CheckinHour = 0
	p2.cfg.CheckinMinute = 10
	p2.cfg.CheckinJitterMin = 30
	p2.applyStoredCheckinSettings(ctx)
	if p2.cfg.CheckinEnable || p2.cfg.CheckinHour != 13 || p2.cfg.CheckinMinute != 5 || p2.cfg.CheckinJitterMin != 20 {
		t.Fatalf("重启后未恢复存储值: %+v", p2.cfg)
	}
}

// 越界的签到时间会让 plannedTime 算到别的日期，必须拒绝。
func TestCheckinSettingsRejectOutOfRange(t *testing.T) {
	cases := []string{
		`{"checkinHour":24}`,
		`{"checkinHour":-1}`,
		`{"checkinMinute":60}`,
		`{"checkinJitterMinutes":2000}`,
	}
	for _, body := range cases {
		p, _, _ := newTestPanel(t)
		if w := putSettings(t, p, body); w.Code != 400 {
			t.Fatalf("%s 应被拒绝，实际 %d", body, w.Code)
		}
	}
}

// 暂停阈值与解除阈值相等时规则自相矛盾（状态每分钟翻转），必须拒绝。
func TestCreditThresholdsRejectEqualWhenEnabled(t *testing.T) {
	p, _, _ := newTestPanel(t)
	w := putSettings(t, p, `{"creditStopEnable":true,"creditStopBelow":100,"creditResumeAbove":100}`)
	if w.Code != 400 {
		t.Fatalf("相等阈值应被拒绝，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "必须大于") {
		t.Fatalf("错误提示不明确: %s", w.Body.String())
	}
	// 规则关闭时允许相等（不生效）
	w = putSettings(t, p, `{"creditStopEnable":false,"creditStopBelow":100,"creditResumeAbove":100}`)
	if w.Code != 200 {
		t.Fatalf("关闭状态下应允许相等: %d %s", w.Code, w.Body.String())
	}
}

// 回滚和安装共用一把门闩：安装进行中再点回滚应返回 409。
func TestRollbackRejectedWhileInstalling(t *testing.T) {
	p, _, _ := newTestPanel(t)
	p.updater = updater.New(updater.Options{Current: "dev", DataDir: t.TempDir()})
	p.updating = true
	req := httptest.NewRequest(http.MethodPost, "/api/update/rollback", nil)
	req.Header.Set("Authorization", "Bearer adm")
	w := httptest.NewRecorder()
	p.handleUpdateRollback(w, req)
	if w.Code != 409 {
		t.Fatalf("安装中回滚应返回 409，实际 %d: %s", w.Code, w.Body.String())
	}
}

// 空 ids 的 purge 必须被拒绝：删除不可逆，误发一个空 POST 不该删掉全池失效号。
func TestPurgeDeadRequiresIDs(t *testing.T) {
	p, _, _ := newTestPanel(t)
	for _, body := range []string{`{}`, ``} {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/dead/purge", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer adm")
		w := httptest.NewRecorder()
		p.handlePurgeDead(w, req)
		if w.Code != 400 {
			t.Fatalf("body=%q 应返回 400，实际 %d: %s", body, w.Code, w.Body.String())
		}
	}
}
