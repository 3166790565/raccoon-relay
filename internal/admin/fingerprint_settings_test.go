package admin

import (
	"context"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// fingerprintPanel 构造一个只够处理设置读写的最小面板。
func fingerprintPanel(t *testing.T) (*Panel, context.Context) {
	t.Helper()
	p, _, ctx := newTestPanel(t)
	t.Cleanup(func() { raccoon.SetFingerprint(raccoon.DefaultFingerprint()) })
	raccoon.SetFingerprint(raccoon.DefaultFingerprint())
	return p, ctx
}

// 保存后立即热更新到进程级指纹，并落库。
func TestPutFingerprintHotUpdatesAndPersists(t *testing.T) {
	p, ctx := fingerprintPanel(t)
	w := putSettings(t, p, `{"clientVersion":"2.0.0","webClientVersion":"v2.1.0",`+
		`"clientOSVersion":"11.0.1","clientPlatform":"desktop-windows-arm64"}`)
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	fp := raccoon.CurrentFingerprint()
	if fp.ClientVersion != "2.0.0" || fp.WebClientVersion != "v2.1.0" {
		t.Fatalf("版本号未热更新: %+v", fp)
	}
	if fp.OSVersion != "11.0.1" || fp.DesktopPlatform != "desktop-windows-arm64" {
		t.Fatalf("系统版本/平台未热更新: %+v", fp)
	}
	if fp.ChatUA() != "raccoon-ai/2.0.0" {
		t.Fatalf("UA 应随版本自动派生，实际 %s", fp.ChatUA())
	}
	if raw := p.st.GetSetting(ctx, "clientVersion", ""); raw != "2.0.0" {
		t.Fatalf("clientVersion 未持久化，实际 %q", raw)
	}
	// GET 要返回当前生效值，前端据此回填。
	got := getSettings(t, p)
	if got["clientVersion"] != "2.0.0" || got["clientOSVersion"] != "11.0.1" {
		t.Fatalf("GET 未返回生效值: %v", got)
	}
}

// UA 留空是合法配置，表示按版本号派生；必须能被持久化成空串。
func TestPutFingerprintAllowsEmptyUserAgent(t *testing.T) {
	p, ctx := fingerprintPanel(t)
	_ = putSettings(t, p, `{"clientUserAgent":"custom-ua/1"}`)
	if got := raccoon.CurrentFingerprint().ChatUA(); got != "custom-ua/1" {
		t.Fatalf("显式 UA 未生效，实际 %s", got)
	}

	if w := putSettings(t, p, `{"clientUserAgent":""}`); w.Code != 200 {
		t.Fatalf("空 UA 应被接受: %d %s", w.Code, w.Body.String())
	}
	fp := raccoon.CurrentFingerprint()
	if fp.ChatUserAgent != "" {
		t.Fatalf("原始值应是空串（自动派生），实际 %q", fp.ChatUserAgent)
	}
	if fp.ChatUA() != raccoon.DeriveUserAgent(fp.ClientVersion) {
		t.Fatalf("空 UA 应回落为按版本派生，实际 %s", fp.ChatUA())
	}
	// 库里要真的存成空串，而不是「没有这个键」——否则重启后会退回出厂 UA。
	raw, ok := p.st.GetSettingOK(ctx, "clientUserAgent")
	if !ok || raw != "" {
		t.Fatalf("空 UA 应持久化为空串，ok=%v raw=%q", ok, raw)
	}
}

// 版本号留空、或夹带控制字符，整体拒绝且不改动现有配置。
func TestPutFingerprintRejectsInvalid(t *testing.T) {
	p, _ := fingerprintPanel(t)
	_ = putSettings(t, p, `{"clientVersion":"2.0.0"}`)

	if w := putSettings(t, p, `{"clientVersion":""}`); w.Code != 400 {
		t.Fatalf("空版本号应拒绝，实际 %d", w.Code)
	}
	if w := putSettings(t, p, `{"clientPlatform":"desk\ntop"}`); w.Code != 400 {
		t.Fatalf("含换行的平台应拒绝，实际 %d", w.Code)
	}
	if w := putSettings(t, p, `{"webUserAgent":"ua\r\nX-Injected: 1"}`); w.Code != 400 {
		t.Fatalf("含换行的 UA 应拒绝，实际 %d", w.Code)
	}
	if got := raccoon.CurrentFingerprint().ClientVersion; got != "2.0.0" {
		t.Fatalf("非法保存不应改动现有配置，实际 %s", got)
	}
}

// 重启后面板保存的值要盖回出厂默认。
func TestApplyStoredFingerprintSettings(t *testing.T) {
	p, ctx := fingerprintPanel(t)
	_ = p.st.SetSetting(ctx, "clientVersion", "3.1.4")
	_ = p.st.SetSetting(ctx, "clientOSVersion", "12.0.0")
	_ = p.st.SetSetting(ctx, "clientUserAgent", "custom-ua/3")

	p.applyStoredFingerprintSettings(ctx)
	fp := raccoon.CurrentFingerprint()
	if fp.ClientVersion != "3.1.4" || fp.OSVersion != "12.0.0" {
		t.Fatalf("持久化值未恢复: %+v", fp)
	}
	if fp.ChatUA() != "custom-ua/3" {
		t.Fatalf("持久化的 UA 未恢复，实际 %s", fp.ChatUA())
	}
}

// 库里存的是空 UA 时，重启后仍应保持「自动派生」，而不是被当成缺失跳过。
func TestApplyStoredFingerprintKeepsEmptyUA(t *testing.T) {
	p, ctx := fingerprintPanel(t)
	_ = p.st.SetSetting(ctx, "clientVersion", "4.0.0")
	_ = p.st.SetSetting(ctx, "clientUserAgent", "someone-else/1")

	p.applyStoredFingerprintSettings(ctx)
	if raccoon.CurrentFingerprint().ChatUA() != "someone-else/1" {
		t.Fatal("前置条件不成立")
	}

	_ = p.st.SetSetting(ctx, "clientUserAgent", "")
	p.applyStoredFingerprintSettings(ctx)
	if got := raccoon.CurrentFingerprint().ChatUA(); got != "raccoon-ai/4.0.0" {
		t.Fatalf("空 UA 应派生为 raccoon-ai/4.0.0，实际 %s", got)
	}
}
