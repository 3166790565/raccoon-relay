package raccoon

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Identity 是每号固定的设备指纹。派生算法与桌面号池一致：
// sha256("agenthub:raccoon:"+uid) → 8-4-4-4-12 形态，第 13 位固定 4、第 17 位固定 a。
// 保持同一算法是为了导入旧号池后指纹不漂移（上游会把突变指纹当异常设备）。
type Identity struct {
	DeviceID       string
	DeviceName     string
	OSVersion      string
	Platform       string
	PlatformNoArch string
	ClientChannel  string
	OfficeIdentity string
	Language       string
}

func DeriveDeviceID(uid string) string {
	if strings.TrimSpace(uid) == "" {
		uid = "anon"
	}
	sum := sha256.Sum256([]byte("agenthub:raccoon:" + uid))
	return deriveFromSeed(hex.EncodeToString(sum[:]))
}

func deriveFromSeed(seed string) string {
	if len(seed) < 32 {
		seed = (seed + strings.Repeat("0", 32))[:32]
	}
	return seed[0:8] + "-" + seed[8:12] + "-4" + seed[13:16] + "-a" + seed[17:20] + "-" + seed[20:32]
}

// IdentityFor 组装账号指纹：meta 里有值就用 meta 的，缺省按 uid 派生。
func IdentityFor(uid, deviceID, osVersion, platform, clientChannel, officeIdentity, language string) Identity {
	seedSum := sha256.Sum256([]byte("agenthub:raccoon:" + defaultStr(uid, "anon")))
	seed := hex.EncodeToString(seedSum[:])

	did := strings.TrimSpace(deviceID)
	if did == "" {
		did = deriveFromSeed(seed)
	}
	plat := strings.TrimSpace(platform)
	if plat == "" {
		plat = "desktop-windows-x64"
	}
	osv := strings.TrimSpace(osVersion)
	if osv == "" {
		osv = "10.0.26200"
	}
	ch := strings.TrimSpace(clientChannel)
	if ch == "" {
		ch = "stable"
	}
	lang := strings.TrimSpace(language)
	if lang == "" {
		lang = "zh"
	}
	name := "DESKTOP-" + strings.ToUpper(sanitizeAlnum(seed[:7]))
	return Identity{
		DeviceID:       did,
		DeviceName:     name,
		OSVersion:      osv,
		Platform:       plat,
		PlatformNoArch: strings.TrimSuffix(strings.TrimSuffix(plat, "-x64"), "-arm64"),
		ClientChannel:  ch,
		OfficeIdentity: strings.TrimSpace(officeIdentity),
		Language:       lang,
	}
}

func sanitizeAlnum(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('X')
		}
	}
	return b.String()
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
