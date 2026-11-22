package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/autoclaw"
	"github.com/3166790565/raccoon-relay/internal/codearts"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// AutoClawImporter 处理 AutoClaw 的登录与凭据导入。
type AutoClawImporter struct {
	C  *autoclaw.Client
	ST *store.Store
}

// CodeArtsImporter 处理 CodeArts 的 OAuth 与凭据导入。
type CodeArtsImporter struct {
	C  *codearts.Client
	ST *store.Store
}

// ImportResult 是外部渠道导入的统一回执。
type importOutcome struct {
	OK      bool
	ID      string
	UID     string
	Updated bool
	Message string
}

// ImportAutoClaw 导入一份 AutoClaw 凭据。
//
// 接受两种形态：
//   - magpie / OpenCode 的 auth 记录：{"access": "...", "refresh": "...", "expires": 毫秒}
//   - 本项目的导出包条目：{"token","refreshToken","extra","site"}
func (im *AutoClawImporter) Import(ctx context.Context, raw, site, name, note string) importOutcome {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return importOutcome{Message: "凭据不能为空"}
	}
	cred := parseAutoClawCredential(raw)
	if cred.Access == "" && cred.Refresh == "" {
		return importOutcome{Message: "凭据里没有 access / refresh token"}
	}
	if site == "" {
		site = store.StationGlobal
	}
	if site != store.StationGlobal && site != store.StationCN {
		return importOutcome{Message: "站点只能是 global 或 cn"}
	}
	acc := autoclaw.BuildAccount(cred, site, strings.TrimSpace(name))
	if note != "" {
		acc.Note = note
	}
	existing, err := im.ST.GetByProviderUID(ctx, store.ProviderAutoClaw, acc.UID)
	if err != nil {
		return importOutcome{Message: "查询失败: " + err.Error()}
	}
	if existing != nil {
		extra := acc.ExtExtra
		patch := store.AccountPatch{
			ExtToken:        &acc.ExtToken,
			ExtTokenExp:     &acc.ExtTokenExp,
			ExtRefreshToken: &acc.ExtRefreshToken,
			ExtSite:         &site,
			ExtExtra:        &extra,
		}
		if strings.TrimSpace(name) != "" {
			patch.Name = &name
		}
		if note != "" {
			patch.Note = &note
		}
		if err := im.ST.UpdateAccount(ctx, existing.ID, patch); err != nil {
			return importOutcome{Message: "更新失败: " + err.Error()}
		}
		return importOutcome{OK: true, ID: existing.ID, UID: acc.UID, Updated: true, Message: "AutoClaw 账号已存在，凭据已更新"}
	}
	if err := im.ST.CreateAccount(ctx, acc); err != nil {
		return importOutcome{Message: "入库失败: " + err.Error()}
	}
	return importOutcome{OK: true, ID: acc.ID, UID: acc.UID, Message: "AutoClaw 账号已添加"}
}

// parseAutoClawCredential 兼容几种常见凭据形态。
func parseAutoClawCredential(raw string) autoclaw.Credential {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		// 不是 JSON：把整串当作 access token，没有续期材料也能用一阵。
		return autoclaw.Credential{Access: strings.TrimSpace(raw), Source: "paste"}
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	cred := autoclaw.Credential{
		Access:   get("access", "access_token", "token"),
		Refresh:  get("refresh", "refresh_token", "refreshToken"),
		UserID:   get("uid", "user_id"),
		Name:     get("name", "email", "nickname"),
		DeviceID: get("device_id"),
		Source:   strings.TrimSpace(strOfAny(m["source"])),
	}
	// magpie 的 auth 记录把到期时刻放在 expires（毫秒，已含提前量）；
	// 本项目导出包放在 tokenExp。
	if n, ok := m["expires"].(float64); ok && n > 0 {
		cred.Expires = int64(n)
	} else if n, ok := m["tokenExp"].(float64); ok && n > 0 {
		cred.Expires = int64(n)
	}
	// 导出包形态：extra 里是完整凭据。
	if extra, ok := m["extra"].(string); ok && strings.TrimSpace(extra) != "" {
		var nested autoclaw.Credential
		if json.Unmarshal([]byte(extra), &nested) == nil {
			if nested.Access != "" {
				cred.Access = nested.Access
			}
			if nested.Refresh != "" {
				cred.Refresh = nested.Refresh
			}
			if nested.Expires != 0 {
				cred.Expires = nested.Expires
			}
			cred.DeviceID = firstNonEmptyStr(cred.DeviceID, nested.DeviceID)
		}
	}
	if cred.Source == "" {
		cred.Source = "paste"
	}
	return cred
}

// ImportCodeArts 导入一份码道凭据。
//
// 接受：
//   - 插件导出的凭据 JSON（access_key_id / secret_access_key / security_token /
//     expires_at / refresh_token / code_verifier / dpop_private_key_jwk）；
//   - 仅 AK/SK/ST 三元组时也能导入，但没有续期材料，到期后需要重新导入。
func (im *CodeArtsImporter) Import(ctx context.Context, raw, site, name, note string) importOutcome {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return importOutcome{Message: "凭据不能为空"}
	}
	var cred codearts.Credential
	if err := json.Unmarshal([]byte(raw), &cred); err != nil || cred.AccessKeyID == "" {
		return importOutcome{Message: "无法解析码道凭据（需要 access_key_id / secret_access_key / security_token）"}
	}
	if cred.Station == "" {
		cred.Station = site
	}
	if cred.Station == "" {
		cred.Station = store.StationGlobal
	}
	if cred.Station != store.StationGlobal && cred.Station != store.StationCN {
		return importOutcome{Message: "站点只能是 global 或 cn"}
	}
	if cred.SessionSeed == "" {
		cred.SessionSeed = randomHexStr(16)
	}
	if cred.Source == "" {
		cred.Source = "paste"
	}
	uid := firstNonEmptyStr(cred.DomainID, cred.UserID, codeartsUIDFromRefresh(cred.RefreshToken))
	if uid == "" {
		uid = "codearts-" + randomHexStr(6)
	}
	display := strings.TrimSpace(name)
	if display == "" {
		display = firstNonEmptyStr(cred.UserName, cred.Phone, uid)
	}

	patch := cred.Patch()
	existing, err := im.ST.GetByProviderUID(ctx, store.ProviderCodeArts, uid)
	if err != nil {
		return importOutcome{Message: "查询失败: " + err.Error()}
	}
	if existing != nil {
		if strings.TrimSpace(name) != "" {
			patch.Name = &name
		}
		if note != "" {
			patch.Note = &note
		}
		if err := im.ST.UpdateAccount(ctx, existing.ID, patch); err != nil {
			return importOutcome{Message: "更新失败: " + err.Error()}
		}
		return importOutcome{OK: true, ID: existing.ID, UID: uid, Updated: true, Message: "码道账号已存在，凭据已更新"}
	}
	acc := &store.Account{
		UID:             uid,
		Name:            display,
		Note:            note,
		Provider:        store.ProviderCodeArts,
		Credits:         -1,
		Status:          store.StatusOnline,
		Language:        "zh",
		Meta:            map[string]any{"source": cred.Source},
		Enabled:         true,
		ExtToken:        cred.AccessKeyID + ":" + cred.ExpiresAt,
		ExtTokenExp:     cred.ExpiryMillis(),
		ExtRefreshToken: cred.RefreshToken,
		ExtSite:         cred.Station,
		ExtExtra:        mustJSONString(cred),
	}
	if err := im.ST.CreateAccount(ctx, acc); err != nil {
		return importOutcome{Message: "入库失败: " + err.Error()}
	}
	msg := "码道账号已添加"
	if cred.RefreshToken == "" {
		msg += "（没有续期材料，凭据到期后需要重新导入）"
	}
	return importOutcome{OK: true, ID: acc.ID, UID: uid, Message: msg}
}

func codeartsUIDFromRefresh(token string) string {
	urn, _ := codearts.ParseRefreshIdentity(token)
	return urn
}

// ---------- 小工具 ----------

func mustJSONString(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func randomHexStr(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func strOfAny(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
