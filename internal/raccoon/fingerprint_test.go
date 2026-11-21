package raccoon

import (
	"strings"
	"testing"
)

// resetFingerprint 让每个测试结束后恢复出厂指纹，避免进程级状态串到别的用例。
func resetFingerprint(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { SetFingerprint(DefaultFingerprint()) })
}

// 对话头必须显式带 User-Agent：Go 缺省补的 Go-http-client/1.1
// 与「自称桌面客户端」的其余头自相矛盾。
func TestChatHeadersCarryUserAgent(t *testing.T) {
	resetFingerprint(t)
	SetFingerprint(DefaultFingerprint())
	idn := IdentityFor("pu-x", "", "", "", "", "", "")

	h := chatHeaders("tok", idn, "sess", "turn", "")
	ua := h["user-agent"]
	if ua == "" {
		t.Fatal("对话头必须带 user-agent")
	}
	if strings.Contains(ua, "Go-http-client") {
		t.Fatalf("不应出现 Go 缺省 UA，实际 %s", ua)
	}
	if ua != DeriveUserAgent(ClientVersion) {
		t.Fatalf("UA 应与版本号一致，实际 %s", ua)
	}
}

// 四套头都要带 UA，否则管理面/刷新/手机端仍然会漏成 Go-http-client。
func TestAllHeaderSetsCarryUserAgent(t *testing.T) {
	resetFingerprint(t)
	SetFingerprint(DefaultFingerprint())
	idn := IdentityFor("pu-x", "", "", "", "", "", "")

	cases := map[string]map[string]string{
		"web":     webHeaders("tok", idn, ""),
		"refresh": refreshHeaders(idn),
		"mobile":  mobileHeaders("tok", idn),
		"bind":    bindHeaders("tok", "dev"),
	}
	for name, h := range cases {
		if strings.TrimSpace(h["user-agent"]) == "" {
			t.Fatalf("%s 头缺少 user-agent", name)
		}
	}
}

// 面板改了版本号，UA 要跟着变——否则会留下「新版本号 + 旧 UA」的自相矛盾。
func TestFingerprintVersionDrivesUA(t *testing.T) {
	resetFingerprint(t)
	SetFingerprint(Fingerprint{
		ClientVersion:    "9.9.9",
		WebClientVersion: "v8.8.8",
		// UA 留空 = 自动派生
	})
	fp := CurrentFingerprint()
	if fp.ChatUA() != "raccoon-ai/9.9.9" {
		t.Fatalf("对话 UA 应随版本派生，实际 %s", fp.ChatUA())
	}
	if fp.WebUA() != "raccoon-ai/8.8.8" {
		t.Fatalf("管理面 UA 应随版本派生（去掉 v 前缀），实际 %s", fp.WebUA())
	}
	idn := IdentityFor("pu-x", "", "", "", "", "", "")
	if got := chatHeaders("tok", idn, "s", "t", "")["x-client-version"]; got != "9.9.9" {
		t.Fatalf("对话头版本未热更新，实际 %s", got)
	}
}

// 显式填写的 UA 优先于自动派生。
func TestFingerprintExplicitUAWins(t *testing.T) {
	resetFingerprint(t)
	SetFingerprint(Fingerprint{
		ClientVersion: "1.0.35",
		ChatUserAgent: "raccoon-ai/1.0.35 (Windows NT 10.0; Win64; x64)",
	})
	if got := CurrentFingerprint().ChatUA(); got != "raccoon-ai/1.0.35 (Windows NT 10.0; Win64; x64)" {
		t.Fatalf("显式 UA 应保留，实际 %s", got)
	}
}

// 空指纹归一后必须还是完整可用的一套值。
func TestFingerprintNormalizedFillsDefaults(t *testing.T) {
	fp := Fingerprint{}.Normalized()
	if fp.ClientVersion != ClientVersion || fp.OSVersion != DefaultOSVersion || fp.DesktopPlatform != DefaultDesktopPlat {
		t.Fatalf("空指纹未回落到出厂默认: %+v", fp)
	}
	if fp.ChatUA() == "" || fp.WebUA() == "" {
		t.Fatalf("空指纹必须能派生出 UA: %+v", fp)
	}
}

// 新号缺省平台/系统版本应当取当前指纹，而不是写死常量。
func TestIdentityForUsesCurrentFingerprintDefaults(t *testing.T) {
	resetFingerprint(t)
	SetFingerprint(Fingerprint{OSVersion: "11.0.1", DesktopPlatform: "desktop-windows-arm64"})
	idn := IdentityFor("pu-x", "", "", "", "", "", "")
	if idn.OSVersion != "11.0.1" {
		t.Fatalf("系统版本应取当前指纹，实际 %s", idn.OSVersion)
	}
	if idn.Platform != "desktop-windows-arm64" {
		t.Fatalf("平台应取当前指纹，实际 %s", idn.Platform)
	}
	if idn.PlatformNoArch != "desktop-windows" {
		t.Fatalf("去架构应得到 desktop-windows，实际 %s", idn.PlatformNoArch)
	}
}
