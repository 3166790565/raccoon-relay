package creds

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// File 是桌面版导出的凭据包。字段顺序与样例保持一致，导出后可直接回灌桌面版。
type File struct {
	Format     string    `json:"format"`
	ExportedAt string    `json:"exportedAt,omitempty"`
	Channel    string    `json:"channel"`
	Accounts   []Account `json:"accounts"`
}

type Account struct {
	Channel      string         `json:"channel"`
	UID          string         `json:"uid"`
	Name         string         `json:"name"`
	Note         string         `json:"note"`
	Token        string         `json:"token"`
	RefreshToken string         `json:"refreshToken"`
	TokenExp     int64          `json:"tokenExp"`
	ExpiresAt    int64          `json:"expiresAt"`
	Credits      int64          `json:"credits"`
	Status       string         `json:"status"`
	Source       string         `json:"source"`
	CreatedAt    int64          `json:"createdAt"`
	UpdatedAt    int64          `json:"updatedAt"`
	Meta         map[string]any `json:"meta"`
}

const (
	FormatCredentials = "agenthub-credentials"
	FormatMetadata    = "agenthub-accounts"
	ChannelRaccoon    = "raccoon"
)

// ImportResult 逐条回传结果，失败不影响其它条。
type ImportResult struct {
	OK      bool   `json:"ok"`
	UID     string `json:"uid,omitempty"`
	ID      string `json:"id,omitempty"`
	Updated bool   `json:"updated,omitempty"`
	Message string `json:"message,omitempty"`
}

// ParseImport 接受三种形态：单个账号对象、账号数组、{channel,accounts:[]}。
func ParseImport(raw []byte) ([]Account, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("内容为空")
	}

	var probe map[string]any
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal(raw, &probe); err == nil {
			if f, _ := probe["format"].(string); f == FormatMetadata {
				return nil, fmt.Errorf("这是账号元数据包（无 token），不能用于导入凭据")
			}
			if arr, ok := probe["accounts"].([]any); ok {
				return decodeList(arr)
			}
		}
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []any
		if err := json.Unmarshal(raw, &arr); err == nil {
			return decodeList(arr)
		}
	}
	// 单个账号对象
	var one Account
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("无法解析导入内容: %w", err)
	}
	return []Account{one}, nil
}

func decodeList(arr []any) ([]Account, error) {
	out := make([]Account, 0, len(arr))
	for i, item := range arr {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条序列化失败: %w", i+1, err)
		}
		var a Account
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("第 %d 条格式非法: %w", i+1, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// Importer 把解析结果写进号池。按 uid 去重，已存在则合并凭据与 meta。
type Importer struct {
	ST *store.Store
}

func (im *Importer) Import(ctx context.Context, list []Account) []ImportResult {
	results := make([]ImportResult, 0, len(list))
	for i, a := range list {
		r := im.importOne(ctx, a)
		if r.Message == "" {
			r.Message = fmt.Sprintf("第 %d 条", i+1)
		}
		results = append(results, r)
	}
	return results
}

func (im *Importer) importOne(ctx context.Context, a Account) ImportResult {
	if a.Channel != "" && a.Channel != ChannelRaccoon {
		return ImportResult{Message: "渠道不是 raccoon，已跳过"}
	}
	token := strings.TrimSpace(a.Token)
	if token == "" {
		return ImportResult{Message: "缺少 token，已跳过"}
	}
	uid := strings.TrimSpace(a.UID)
	if uid == "" {
		uid = raccoon.TokenUID(token)
	}
	if uid == "" {
		return ImportResult{Message: "无法从 token 解析账号 uid，已跳过"}
	}

	tokenExp := a.TokenExp
	if tokenExp == 0 {
		if c, err := raccoon.DecodeJWT(token); err == nil && c.Exp > 0 {
			tokenExp = c.Exp * 1000
		}
	}

	meta := a.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	officeIdentity := strFromMeta(meta, "officeIdentity")
	deviceID := strFromMeta(meta, "deviceId")
	if deviceID == "" {
		deviceID = raccoon.DeriveDeviceID(uid)
		meta["deviceId"] = deviceID
	}
	if _, ok := meta["officeIdentity"]; !ok {
		meta["officeIdentity"] = officeIdentity
	}

	existing, err := im.ST.GetByUID(ctx, uid)
	if err != nil {
		return ImportResult{UID: uid, Message: "查询失败: " + err.Error()}
	}

	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = "账号 " + shortUID(uid)
	}
	status := strings.TrimSpace(a.Status)
	if status == "" {
		status = store.StatusOnline
	}

	if existing != nil {
		merged := mergeMeta(existing.Meta, meta)
		p := store.AccountPatch{
			Token:           &token,
			RefreshToken:    &a.RefreshToken,
			TokenExp:        &tokenExp,
			Credits:         &a.Credits,
			Status:          &status,
			ExpiresAt:       &a.ExpiresAt,
			OfficeIdentity:  &officeIdentity,
			DeviceID:        &deviceID,
			Meta:            merged,
		}
		if name != "" {
			p.Name = &name
		}
		if a.Note != "" {
			p.Note = &a.Note
		}
		if status == store.StatusOnline {
			zero := int64(0)
			empty := ""
			p.CoolUntil = &zero
			p.CoolReason = &empty
		}
		if err := im.ST.UpdateAccount(ctx, existing.ID, p); err != nil {
			return ImportResult{UID: uid, Message: "更新失败: " + err.Error()}
		}
		return ImportResult{OK: true, UID: uid, ID: existing.ID, Updated: true}
	}

	acc := &store.Account{
		UID:            uid,
		Name:           name,
		Note:           a.Note,
		Token:          token,
		RefreshToken:   a.RefreshToken,
		TokenExp:       tokenExp,
		Credits:        a.Credits,
		CreditsAt:      nowIfKnown(a.Credits),
		Status:         status,
		ExpiresAt:      a.ExpiresAt,
		DeviceID:       deviceID,
		DeviceName:     raccoon.IdentityFor(uid, deviceID, "", "", "", officeIdentity, "").DeviceName,
		OSVersion:      raccoon.DefaultOSVersion,
		OfficeIdentity: officeIdentity,
		Platform:       raccoon.DefaultDesktopPlat,
		ClientChannel:  "stable",
		Language:       "zh",
		Meta:           meta,
		Enabled:        true,
	}
	if err := im.ST.CreateAccount(ctx, acc); err != nil {
		return ImportResult{UID: uid, Message: "入库失败: " + err.Error()}
	}
	return ImportResult{OK: true, UID: uid, ID: acc.ID}
}

// Export 读全池生成与桌面版同格式的 JSON。导出明文 token（与桌面版行为一致）。
func Export(ctx context.Context, st *store.Store) (*File, error) {
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	f := &File{
		Format:     FormatCredentials,
		ExportedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Channel:    ChannelRaccoon,
		Accounts:   make([]Account, 0, len(accounts)),
	}
	for _, a := range accounts {
		f.Accounts = append(f.Accounts, Account{
			Channel:      ChannelRaccoon,
			UID:          a.UID,
			Name:         a.Name,
			Note:         a.Note,
			Token:        a.Token,
			RefreshToken: a.RefreshToken,
			TokenExp:     a.TokenExp,
			ExpiresAt:    a.ExpiresAt,
			Credits:      a.Credits,
			Status:       a.Status,
			Source:       sourceOf(a),
			CreatedAt:    a.CreatedAt,
			UpdatedAt:    a.UpdatedAt,
			Meta:         a.Meta,
		})
	}
	return f, nil
}

func sourceOf(a *store.Account) string {
	if s, ok := a.Meta["source"].(string); ok && s != "" {
		return s
	}
	return "import"
}

func mergeMeta(old, next map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range old {
		out[k] = v
	}
	for k, v := range next {
		// onboardingGrants 与 checkin 是台账，只增不删
		if k == "onboardingGrants" {
			if prev, ok := out[k].(map[string]any); ok {
				if incoming, ok := v.(map[string]any); ok {
					merged := map[string]any{}
					for kk, vv := range prev {
						merged[kk] = vv
					}
					for kk, vv := range incoming {
						merged[kk] = vv
					}
					out[k] = merged
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}

func strFromMeta(meta map[string]any, key string) string {
	if v, ok := meta[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func shortUID(uid string) string {
	r := []rune(uid)
	if len(r) > 6 {
		return string(r[:6])
	}
	return uid
}

func nowIfKnown(credits int64) int64 {
	if credits >= 0 {
		return time.Now().UnixMilli()
	}
	return 0
}
