package raccoon

import (
	"crypto/rand"
	"strings"
)

// 两套头必须分开，混用会被上游按异常客户端处理：
//   · 对话面：小写 kebab 的 x-client-* 六头 + X-RACCOON-*
//   · 管理面（余额/签到/刷新）：大写 X-Client-*，版本带 v
// 手机端首登奖那条端点额外报 app-android / v1.0.3。

const (
	ClientName         = "raccoon-ai"
	ClientVersion      = "1.0.35"
	WebClientVersion   = "v1.1.3"
	MobilePlatform     = "app-android"
	MobileClientVers   = "v1.0.3"
	DefaultOSVersion   = "10.0.26200"
	DefaultDesktopPlat = "desktop-windows-x64"
)

// chatHeaders 是小浣熊对话端点的头集。title 取首条用户消息前 20 字，非 ASCII percent-encode。
func chatHeaders(token string, idn Identity, sessionID, turnID, title string) map[string]string {
	h := map[string]string{
		"content-type":         "application/json",
		"accept":               "application/json, text/event-stream",
		"authorization":        "Bearer " + token,
		"x-client-name":        ClientName,
		"x-client-platform":    idn.Platform,
		"x-client-version":     ClientVersion,
		"x-client-os-version":  idn.OSVersion,
		"x-client-channel":     idn.ClientChannel,
		"x-client-device-id":   idn.DeviceID,
		"X-RACCOON-Session-ID": sessionID,
		"X-RACCOON-Turn-ID":    turnID,
		"X-RACCOON-Title":      latinTitle(title),
		"X-RACCOON-Call-Kind":  "chat",
	}
	if isTeam(idn.OfficeIdentity) {
		h["X-Org-Code"] = idn.OfficeIdentity
	}
	return h
}

// webHeaders 是余额/签到/用户信息等渲染层请求的头集。platform 传空则用去架构的桌面平台。
func webHeaders(token string, idn Identity, platform string) map[string]string {
	if platform == "" {
		platform = idn.PlatformNoArch
	}
	h := map[string]string{
		"content-type":       "application/json",
		"accept":             "application/json",
		"X-Client-Platform":  platform,
		"X-Client-Version":   WebClientVersion,
		"X-Client-Device-ID": idn.DeviceID,
		"X-Raccoon-Language": idn.Language,
	}
	if token != "" {
		h["authorization"] = "Bearer " + token
	}
	if isTeam(idn.OfficeIdentity) {
		h["X-Org-Code"] = idn.OfficeIdentity
	}
	return h
}

// refreshHeaders 只带 refresh_token，绝不带旧 access——带上去会被 401。
func refreshHeaders(idn Identity) map[string]string {
	h := map[string]string{
		"content-type":       "application/json",
		"accept":             "application/json",
		"X-Client-Platform":  idn.PlatformNoArch,
		"X-Client-Version":   WebClientVersion,
		"X-Client-Device-ID": idn.DeviceID,
	}
	if isTeam(idn.OfficeIdentity) {
		h["X-Org-Code"] = idn.OfficeIdentity
	}
	return h
}

// mobileHeaders 是手机端首登奖端点专用形态。
func mobileHeaders(token string, idn Identity) map[string]string {
	h := map[string]string{
		"content-type":      "application/json",
		"accept":            "application/json",
		"authorization":     "Bearer " + token,
		"X-Client-Platform": MobilePlatform,
		"X-Client-Version":  MobileClientVers,
	}
	if isTeam(idn.OfficeIdentity) {
		h["X-Org-Code"] = idn.OfficeIdentity
	}
	return h
}

// bindHeaders 是可信设备绑定请求的头集。
func bindHeaders(token, deviceID string) map[string]string {
	return map[string]string{
		"content-type":       "application/json",
		"accept":             "application/json",
		"authorization":      "Bearer " + token,
		"X-Client-Platform":  "desktop-windows",
		"X-Client-Device-ID": deviceID,
		"X-Client-Version":   ClientVersion,
	}
}

func isTeam(officeIdentity string) bool {
	oi := strings.TrimSpace(officeIdentity)
	return oi != "" && oi != "personal"
}

// latinTitle 把标题转成纯 ASCII：可打印字符原样，其余 percent-encode。
func latinTitle(title string) string {
	var b strings.Builder
	for _, r := range title {
		if r >= 0x20 && r <= 0x7e {
			b.WriteRune(r)
			continue
		}
		b.WriteString(percentEncodeRune(r))
	}
	return b.String()
}

func percentEncodeRune(r rune) string {
	const hexDigits = "0123456789ABCDEF"
	buf := make([]byte, 0, 3*4)
	for _, by := range []byte(string(r)) {
		buf = append(buf, '%', hexDigits[by>>4], hexDigits[by&0xf])
	}
	return string(buf)
}

// NewTurnID 每次请求一个新的 UUID 形态串。
func NewTurnID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	const hx = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, by := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hx[by>>4], hx[by&0xf])
	}
	return string(out)
}

// SessionID 由 uid + 首条消息稳定派生，让上游把同一会话归到一起。
func SessionID(uid string, seedText string) string {
	s := hashHex("raccoon:" + defaultStr(uid, "anon") + ":" + seedText)
	return s[0:8] + "-" + s[8:12] + "-4" + s[13:16] + "-a" + s[17:20] + "-" + s[20:32]
}
