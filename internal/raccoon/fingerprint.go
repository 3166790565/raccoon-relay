package raccoon

import (
	"strings"
	"sync"
)

// 出厂默认值。它们只是「首次启动的默认」，面板保存后以库里为准。
const (
	ClientName         = "raccoon-ai"
	ClientVersion      = "1.0.35"
	WebClientVersion   = "v1.1.3"
	MobilePlatform     = "app-android"
	MobileClientVers   = "v1.0.3"
	DefaultOSVersion   = "10.0.26200"
	DefaultDesktopPlat = "desktop-windows-x64"
)

// Fingerprint 是发往上游的客户端身份，统一决定：
//   - 对话面 x-client-version / x-client-os-version 与 User-Agent
//   - 管理面（余额/签到/刷新/绑定）X-Client-Version 与 User-Agent
//   - 新账号入库时的默认系统版本与桌面平台
//
// 为什么必须可配：这些值全部来自上游桌面客户端，写死在二进制里就会随上游
// 发版一起过期。「旧版本号 + 全新会话」本身就是第三方客户端的典型特征，
// 所以面板要能随时改成当前版本，而不必重新发版。
//
// ChatUserAgent / WebUserAgent 存的是**用户填的原始值**（可以为空）。
// 空表示「跟着版本号自动派生」——派生发生在读取时（见 ChatUA/WebUA），
// 而不是写入时。否则改了版本号，UA 还会停在上一次的旧值上。
type Fingerprint struct {
	ClientVersion    string
	WebClientVersion string
	OSVersion        string
	DesktopPlatform  string
	MobilePlatform   string
	MobileVersion    string
	ChatUserAgent    string
	WebUserAgent     string
}

// DefaultFingerprint 返回出厂默认指纹（UA 留空 = 按版本号派生）。
func DefaultFingerprint() Fingerprint {
	return Fingerprint{
		ClientVersion:    ClientVersion,
		WebClientVersion: WebClientVersion,
		OSVersion:        DefaultOSVersion,
		DesktopPlatform:  DefaultDesktopPlat,
		MobilePlatform:   MobilePlatform,
		MobileVersion:    MobileClientVers,
	}
}

// DeriveUserAgent 由客户端版本派生 UA。去掉上游版本号前缀的 v，
// 与 x-client-version 的写法保持一致（raccoon-ai/1.1.3）。
func DeriveUserAgent(version string) string {
	return ClientName + "/" + strings.TrimPrefix(strings.TrimSpace(version), "v")
}

// ChatUA 是对话面实际下发的 User-Agent：显式填写优先，否则按版本派生。
func (f Fingerprint) ChatUA() string {
	if ua := strings.TrimSpace(f.ChatUserAgent); ua != "" {
		return ua
	}
	return DeriveUserAgent(f.ClientVersion)
}

// WebUA 是管理面实际下发的 User-Agent：显式填写优先，否则按版本派生。
func (f Fingerprint) WebUA() string {
	if ua := strings.TrimSpace(f.WebUserAgent); ua != "" {
		return ua
	}
	return DeriveUserAgent(f.WebClientVersion)
}

// MobileUA 是手机端首登奖端点用的 UA，按手机端版本派生。
// 它不单独配置：该端点只是一次性领奖，UA 跟着版本走即可。
func (f Fingerprint) MobileUA() string {
	return DeriveUserAgent(f.MobileVersion)
}

// Normalized 补齐空的版本/平台字段，UA 原样保留（空 = 自动派生）。
func (f Fingerprint) Normalized() Fingerprint {
	out := DefaultFingerprint()
	if v := strings.TrimSpace(f.ClientVersion); v != "" {
		out.ClientVersion = v
	}
	if v := strings.TrimSpace(f.WebClientVersion); v != "" {
		out.WebClientVersion = v
	}
	if v := strings.TrimSpace(f.OSVersion); v != "" {
		out.OSVersion = v
	}
	if v := strings.TrimSpace(f.DesktopPlatform); v != "" {
		out.DesktopPlatform = v
	}
	if v := strings.TrimSpace(f.MobilePlatform); v != "" {
		out.MobilePlatform = v
	}
	if v := strings.TrimSpace(f.MobileVersion); v != "" {
		out.MobileVersion = v
	}
	out.ChatUserAgent = strings.TrimSpace(f.ChatUserAgent)
	out.WebUserAgent = strings.TrimSpace(f.WebUserAgent)
	return out
}

var (
	fingerprintMu sync.RWMutex
	fingerprint   = DefaultFingerprint()
)

// SetFingerprint 热更新进程级指纹。空版本字段按默认值补齐，UA 空值保留为「自动派生」。
func SetFingerprint(f Fingerprint) {
	f = f.Normalized()
	fingerprintMu.Lock()
	fingerprint = f
	fingerprintMu.Unlock()
}

// CurrentFingerprint 返回当前生效的指纹（UA 可能是空值，表示自动派生）。
func CurrentFingerprint() Fingerprint {
	fingerprintMu.RLock()
	defer fingerprintMu.RUnlock()
	return fingerprint
}
