package admin

import (
	"testing"
)

// 保存后立刻热更新到 cfg，并落库。
func TestPutExternalSettingsHotUpdatesAndPersists(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	w := putSettings(t, p, `{"autoClawIntlBase":" https://proxy.example.com/autoclaw/ ",`+
		`"codeArtsMainlandBase":"https://proxy.example.com/ca"}`)
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	if got := p.cfg.AutoClawInternationalBase; got != "https://proxy.example.com/autoclaw" {
		t.Fatalf("AutoClaw 国际站未热更新或未去掉尾斜杠: %q", got)
	}
	if got := p.cfg.CodeArtsMainlandBase; got != "https://proxy.example.com/ca" {
		t.Fatalf("码道国内站未热更新: %q", got)
	}
	if raw := p.st.GetSetting(ctx, "autoclawIntlBase", ""); raw != "https://proxy.example.com/autoclaw" {
		t.Fatalf("未持久化: %q", raw)
	}
	got := getSettings(t, p)
	if got["autoClawIntlBase"] != "https://proxy.example.com/autoclaw" {
		t.Fatalf("GET 未返回生效值: %v", got["autoClawIntlBase"])
	}
}

// 空值是合法配置（= 用内置默认），必须能被持久化成空串。
func TestPutExternalSettingsAllowsEmpty(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	_ = putSettings(t, p, `{"autoClawCNBase":"https://x.example.com"}`)
	if w := putSettings(t, p, `{"autoClawCNBase":""}`); w.Code != 200 {
		t.Fatalf("空地址应被接受: %d %s", w.Code, w.Body.String())
	}
	if got := p.cfg.AutoClawCNBase; got != "" {
		t.Fatalf("应清空为默认值，实际 %q", got)
	}
	raw, ok := p.st.GetSettingOK(ctx, "autoclawCNBase")
	if !ok || raw != "" {
		t.Fatalf("空值应持久化成空串，ok=%v raw=%q", ok, raw)
	}
}

// 非法地址整体拒绝，且不改动现有值。
func TestPutExternalSettingsRejectsInvalid(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	_ = putSettings(t, p, `{"codeArtsIntlBase":"https://good.example.com"}`)

	for _, bad := range []string{
		`{"codeArtsMainlandBase":"ftp://bad.example.com"}`,
		`{"codeArtsMainlandBase":"https://user:pass@bad.example.com"}`,
		`{"codeArtsMainlandBase":"https://bad.example.com/?q=1"}`,
		`{"codeArtsMainlandBase":"not a url"}`,
		// 一条合法 + 一条非法：整条都要拒绝。
		`{"autoClawIntlBase":"https://ok.example.com","codeArtsMainlandBase":"javascript:alert(1)"}`,
	} {
		if w := putSettings(t, p, bad); w.Code != 400 {
			t.Fatalf("应拒绝 %s，实际 %d", bad, w.Code)
		}
	}
	if got := p.cfg.CodeArtsInternationalBase; got != "https://good.example.com" {
		t.Fatalf("非法保存不应改动现有值，实际 %q", got)
	}
	if got := p.cfg.AutoClawInternationalBase; got != "" {
		t.Fatalf("整条拒绝时合法项也不该落地，实际 %q", got)
	}
	if raw := p.st.GetSetting(ctx, "autoclawIntlBase", "unset"); raw != "unset" {
		t.Fatalf("整体拒绝时不应写库，实际 %q", raw)
	}
}

// 重启后库里的值要盖回 cfg。
func TestApplyStoredExternalSettings(t *testing.T) {
	p, _, ctx := newTestPanel(t)
	_ = p.st.SetSetting(ctx, "autoclawIntlBase", "https://persisted.example.com")
	_ = p.st.SetSetting(ctx, "codeartsIntlBase", "")
	p.cfg.AutoClawInternationalBase = ""
	p.cfg.CodeArtsInternationalBase = "https://stale.example.com"

	p.applyStoredExternalSettings(ctx)
	if got := p.cfg.AutoClawInternationalBase; got != "https://persisted.example.com" {
		t.Fatalf("未恢复持久化值: %q", got)
	}
	// 空串也是持久化值，不能因为「看起来是空的」就被跳过。
	if got := p.cfg.CodeArtsInternationalBase; got != "" {
		t.Fatalf("空串持久化值应覆盖旧值，实际 %q", got)
	}
}
