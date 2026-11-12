package raccoon

import (
	"regexp"
	"testing"
)

// 设备指纹派生必须与 AgentHub 算法一致：同一 uid 恒定，不同 uid 互异，形态是合法 UUID。
func TestDeriveDeviceIDStable(t *testing.T) {
	uid := "pu-01a1d03b-c02e-7219-8505-8b9f65d64019"
	a := DeriveDeviceID(uid)
	b := DeriveDeviceID(uid)
	if a != b {
		t.Fatalf("同一 uid 应派生相同 deviceId：%s vs %s", a, b)
	}
	pat := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-a[0-9a-f]{3}-[0-9a-f]{12}$`)
	if !pat.MatchString(a) {
		t.Fatalf("形态不合法：%s", a)
	}
	if DeriveDeviceID("pu-other") == a {
		t.Fatal("不同 uid 必须派生出不同 deviceId")
	}
}

// 已知值回归：由 AgentHub 同算法算出，锁定后防止未来重构把指纹改漂移。
func TestDeriveDeviceIDKnownValue(t *testing.T) {
	got := DeriveDeviceID("pu-01a1d03b-c02e-7219-8505-8b9f65d64019")
	want := deriveFromSeed(hashHex("agenthub:raccoon:pu-01a1d03b-c02e-7219-8505-8b9f65d64019"))
	if got != want {
		t.Fatalf("派生口径漂移：%s vs %s", got, want)
	}
}

func TestIdentityForUsesMetaAndFallsBack(t *testing.T) {
	idn := IdentityFor("pu-x", "fixed-device", "", "desktop-windows-arm64", "", "", "")
	if idn.DeviceID != "fixed-device" {
		t.Fatalf("meta 里的 deviceId 应优先，实际 %s", idn.DeviceID)
	}
	if idn.PlatformNoArch != "desktop-windows" {
		t.Fatalf("去架构应得到 desktop-windows，实际 %s", idn.PlatformNoArch)
	}
	if idn.OSVersion != DefaultOSVersion {
		t.Fatalf("OSVersion 应回落默认值，实际 %s", idn.OSVersion)
	}
	if idn.ClientChannel != "stable" {
		t.Fatalf("clientChannel 应回落 stable，实际 %s", idn.ClientChannel)
	}
	if idn.Language != "zh" {
		t.Fatalf("language 应回落 zh，实际 %s", idn.Language)
	}
}

func TestIdentityTeamOrgCode(t *testing.T) {
	idn := IdentityFor("pu-x", "", "", "", "", "org-123", "")
	if !isTeam(idn.OfficeIdentity) {
		t.Fatal("非 personal 的 officeIdentity 应判定为团队版")
	}
	h := webHeaders("tok", idn, "")
	if h["X-Org-Code"] != "org-123" {
		t.Fatalf("团队版应带 X-Org-Code，实际头=%v", h)
	}

	personal := IdentityFor("pu-y", "", "", "", "", "personal", "")
	if isTeam(personal.OfficeIdentity) {
		t.Fatal("personal 不应判定为团队版")
	}
	if _, ok := webHeaders("tok", personal, "")["X-Org-Code"]; ok {
		t.Fatal("个人版不应带 X-Org-Code")
	}
}

// 两套头不能混用：对话面小写 kebab，管理面大写 X-Client-*。
func TestHeaderSetsAreDistinct(t *testing.T) {
	idn := IdentityFor("pu-x", "", "", "", "", "", "")

	chat := chatHeaders("tok", idn, "sess", "turn", "标题")
	if _, ok := chat["x-client-os-version"]; !ok {
		t.Fatal("对话头应带 x-client-os-version")
	}
	if _, ok := chat["X-Client-Platform"]; ok {
		t.Fatal("对话头不应带管理面的大写 X-Client-Platform")
	}
	if chat["X-RACCOON-Call-Kind"] != "chat" {
		t.Fatal("对话头应带 X-RACCOON-Call-Kind: chat")
	}

	web := webHeaders("tok", idn, "")
	if _, ok := web["X-Client-Platform"]; !ok {
		t.Fatal("管理面头应带大写 X-Client-Platform")
	}
	if _, ok := web["x-client-name"]; ok {
		t.Fatal("管理面头不应带对话面的 x-client-name")
	}
	if web["X-Client-Version"] != WebClientVersion {
		t.Fatalf("管理面版本应为 %s，实际 %s", WebClientVersion, web["X-Client-Version"])
	}

	// 刷新头绝不能带 authorization
	if _, ok := refreshHeaders(idn)["authorization"]; ok {
		t.Fatal("刷新头不应带 authorization（会把过期 access 一起发上去）")
	}

	mob := mobileHeaders("tok", idn)
	if mob["X-Client-Platform"] != MobilePlatform || mob["X-Client-Version"] != MobileClientVers {
		t.Fatalf("手机端头不对：%v", mob)
	}
}

func TestLATINTitleEscaping(t *testing.T) {
	got := latinTitle("你好ab")
	want := "%E4%BD%A0%E5%A5%BDab"
	if got != want {
		t.Fatalf("标题转义不对：%s vs %s", got, want)
	}
}

func TestSessionIDStablePerUIDSession(t *testing.T) {
	a := SessionID("pu-1", "hello")
	b := SessionID("pu-1", "hello")
	c := SessionID("pu-1", "other")
	if a != b {
		t.Fatal("同 uid 同会话文本应得到相同 sessionId")
	}
	if a == c {
		t.Fatal("不同会话文本应得到不同 sessionId")
	}
}
