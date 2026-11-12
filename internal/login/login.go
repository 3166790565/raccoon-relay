package login

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"raccoon-relay/internal/raccoon"
	"raccoon-relay/internal/store"
)

// Manager 处理容器内的登录：授权码兑换 + 手填令牌。
// 容器拿不到 office-raccoon:// 深链，所以把「取 code」这一步交给用户浏览器，服务端只做兑换。
type Manager struct {
	ST     *store.Store
	Client *raccoon.Client
	// AuthPageBase 是官方授权页域名。
	AuthPageBase string
	// AppName 是授权页的 appname 参数。
	AppName string
	// RedirectBase 非空时启用回环回调路径。
	RedirectBase string
}

func New(st *store.Store, c *raccoon.Client, authPageBase, appName, redirectBase string) *Manager {
	return &Manager{
		ST:           st,
		Client:       c,
		AuthPageBase: strings.TrimRight(authPageBase, "/"),
		AppName:      appName,
		RedirectBase: strings.TrimRight(redirectBase, "/"),
	}
}

// AuthorizeURL 是面板「打开登录页」按钮跳转的地址。
func (m *Manager) AuthorizeURL() string {
	appName := m.AppName
	if appName == "" {
		appName = "办公小浣熊客户端"
	}
	return fmt.Sprintf("%s/code/authorize?login_source=desktop&appname=%s", m.AuthPageBase, url.QueryEscape(appName))
}

// ExtractCode 从三种输入里抠出授权码：完整深链、裸 code、任意含 code 的地址。
func ExtractCode(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return ""
	}
	// 完整 URL / 深链
	if strings.Contains(s, "://") || strings.Contains(s, "?") {
		if u, err := url.Parse(s); err == nil {
			q := u.Query()
			for _, k := range []string{"code", "authCode", "authorization_code"} {
				if v := q.Get(k); v != "" {
					return v
				}
			}
		}
	}
	// 兜底：正则式手工扫描 code=
	for _, key := range []string{"code=", "authCode=", "authorization_code="} {
		if i := strings.Index(s, key); i >= 0 {
			rest := s[i+len(key):]
			if j := strings.IndexAny(rest, "&# \n\r\t"); j >= 0 {
				rest = rest[:j]
			}
			if rest != "" {
				return rest
			}
		}
	}
	// 裸 code（无分隔符的整串）
	if !strings.ContainsAny(s, " /\\:?#&") {
		return s
	}
	return ""
}

// ExchangeResult 是兑换入池的结论。
type ExchangeResult struct {
	OK      bool
	ID      string
	UID     string
	Updated bool
	Message string
}

// Exchange 用授权码换 token 并入池。code 一次性，失败给可执行提示。
func (m *Manager) Exchange(ctx context.Context, rawInput string) ExchangeResult {
	code := ExtractCode(rawInput)
	if code == "" {
		return ExchangeResult{Message: "无法从粘贴的内容里提取授权码：请整段复制浏览器地址栏内容（形如 office-raccoon://auth/callback?code=…）"}
	}
	ex, err := m.Client.ExchangeAuthCode(ctx, code)
	if err != nil {
		return ExchangeResult{Message: err.Error()}
	}
	uid := raccoon.DecodeUID(ex.Token)
	if uid == "" {
		return ExchangeResult{Message: "授权码已兑换但 token 解不出账号 uid，未入池"}
	}
	deviceID := raccoon.DeriveDeviceID(uid)
	// 可信设备绑定失败不阻断，只是少一层设备信任
	_ = m.Client.BindDevice(ctx, ex.Token, deviceID)

	return m.upsert(ctx, AccountInput{
		UID:           uid,
		Token:         ex.Token,
		RefreshToken:  ex.RefreshToken,
		OfficeIdentity: ex.OfficeID,
		DeviceID:      deviceID,
		Source:        "oauth",
	})
}

// AccountInput 是两条登录路径共用的入库参数。
type AccountInput struct {
	UID            string
	Name           string
	Token          string
	RefreshToken   string
	OfficeIdentity string
	DeviceID       string
	Source         string
}

// Manual 走手填令牌路径：解 JWT 取 uid 与 exp，解不出就拒绝，不建脏号。
func (m *Manager) Manual(ctx context.Context, in AccountInput) ExchangeResult {
	token := strings.TrimSpace(in.Token)
	if token == "" {
		return ExchangeResult{Message: "access_token 不能为空"}
	}
	claims, err := raccoon.DecodeJWT(token)
	if err != nil {
		return ExchangeResult{Message: "access_token 不是合法 JWT，请确认复制完整"}
	}
	uid := strings.TrimSpace(claims.Sub)
	if uid == "" {
		return ExchangeResult{Message: "token 里没有 sub 声明，无法确定账号，已拒绝入库"}
	}
	deviceID := strings.TrimSpace(in.DeviceID)
	if deviceID == "" {
		deviceID = raccoon.DeriveDeviceID(uid)
	}
	in.UID = uid
	in.DeviceID = deviceID
	in.Source = "manual"
	return m.upsert(ctx, in)
}

func (m *Manager) upsert(ctx context.Context, in AccountInput) ExchangeResult {
	existing, err := m.ST.GetByUID(ctx, in.UID)
	if err != nil {
		return ExchangeResult{Message: "查询失败: " + err.Error()}
	}

	tokenExp := raccoon.TokenExpiryMillis(in.Token)
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "账号 " + short(in.UID)
	}

	if existing != nil {
		// 历史版本的 meta 可能把 iss 常量当过 uid，用 officeIdentity 区分同 uid 不同号
		token := in.Token
		ref := in.RefreshToken
		office := in.OfficeIdentity
		status := store.StatusOnline
		zero := int64(0)
		empty := ""
		meta := existing.Meta
		if meta == nil {
			meta = map[string]any{}
		}
		if office != "" {
			meta["officeIdentity"] = office
		}
		if existing.DeviceID == "" && in.DeviceID != "" {
			meta["deviceId"] = in.DeviceID
		}
		err := m.ST.UpdateAccount(ctx, existing.ID, store.AccountPatch{
			Token: &token, RefreshToken: &ref, TokenExp: &tokenExp,
			Status: &status, CoolUntil: &zero, CoolReason: &empty,
			OfficeIdentity: &office, Meta: meta,
		})
		if err != nil {
			return ExchangeResult{UID: in.UID, Message: "更新失败: " + err.Error()}
		}
		return ExchangeResult{OK: true, ID: existing.ID, UID: in.UID, Updated: true, Message: "账号已存在，凭据已更新"}
	}

	idn := raccoon.IdentityFor(in.UID, in.DeviceID, "", "", "", in.OfficeIdentity, "")
	meta := map[string]any{
		"officeIdentity": in.OfficeIdentity,
		"deviceId":       in.DeviceID,
		"source":         in.Source,
	}
	acc := &store.Account{
		UID:            in.UID,
		Name:           name,
		Token:          in.Token,
		RefreshToken:   in.RefreshToken,
		TokenExp:       tokenExp,
		Credits:        -1,
		Status:         store.StatusOnline,
		DeviceID:       in.DeviceID,
		DeviceName:     idn.DeviceName,
		OSVersion:      idn.OSVersion,
		OfficeIdentity: in.OfficeIdentity,
		Platform:       idn.Platform,
		ClientChannel:  idn.ClientChannel,
		Language:       "zh",
		Meta:           meta,
		Enabled:        true,
	}
	if err := m.ST.CreateAccount(ctx, acc); err != nil {
		return ExchangeResult{UID: in.UID, Message: "入库失败: " + err.Error()}
	}
	return ExchangeResult{OK: true, ID: acc.ID, UID: in.UID, Message: "账号已添加"}
}

func short(uid string) string {
	r := []rune(uid)
	if len(r) > 6 {
		return string(r[:6])
	}
	return uid
}
