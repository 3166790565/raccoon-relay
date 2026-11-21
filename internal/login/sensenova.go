package login

import (
	"context"
	"strings"

	"github.com/3166790565/raccoon-relay/internal/sensenova"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// SenseNovaInput 是「商汤日日新账号密码导入」的入参。
type SenseNovaInput struct {
	Username string // 控制台账号（手机号/邮箱）
	Password string // 控制台密码
	Name     string // 可选备注名
	Note     string
	// APIKey 可选：用户已经从控制台创建好 Key 时可以直接填，跳过建 Key。
	APIKey string
}

// SenseNovaImporter 处理日日新账号的「账号密码 → 登录票 → API Key」导入。
type SenseNovaImporter struct {
	ST *store.Store
	C  *sensenova.Client
}

func NewSenseNovaImporter(st *store.Store, c *sensenova.Client) *SenseNovaImporter {
	return &SenseNovaImporter{ST: st, C: c}
}

// Import 登录取票、建 Key、入库。返回结果供面板回显。
//
// 去重口径：先按控制台账号（SNUsername）找已有号，没有再用登录票里的
// tenant_id 兜底——同一账号重复导入时只更新凭据，不新建行。
func (im *SenseNovaImporter) Import(ctx context.Context, in SenseNovaInput) ExchangeResult {
	in.Username = strings.TrimSpace(in.Username)
	in.Password = strings.TrimSpace(in.Password)
	in.Name = strings.TrimSpace(in.Name)
	in.Note = strings.TrimSpace(in.Note)
	in.APIKey = strings.TrimSpace(in.APIKey)
	if in.Username == "" || in.Password == "" {
		return ExchangeResult{Message: "账号和密码都不能为空"}
	}

	loginRes, err := im.C.Login(ctx, in.Username, in.Password)
	if err != nil {
		return ExchangeResult{Message: "登录失败：" + err.Error()}
	}
	tenant := loginRes.TenantID
	if tenant == "" {
		tenant = in.Username
	}

	apiKey := in.APIKey
	if apiKey == "" {
		key, kerr := im.C.CreateAPIKey(ctx, loginRes.AccessToken, "raccoon-relay-sensenova")
		if kerr != nil {
			// 面板有控制台登录票时，至少先把号建起来（额度可查），
			// Key 留空，首次转发时会再尝试补建。
			apiKey = ""
		} else {
			apiKey = key
		}
	}

	existing, err := im.findExisting(ctx, tenant, in.Username)
	if err != nil {
		return ExchangeResult{Message: "查询失败: " + err.Error()}
	}

	display := in.Name
	if display == "" && loginRes.Username != "" {
		display = loginRes.Username
	}
	if display == "" {
		display = in.Username
	}

	if existing != nil {
		patch := store.AccountPatch{
			SNUsername:     &in.Username,
			SNPassword:     &in.Password,
			SNToken:        &loginRes.AccessToken,
			SNTokenExp:     &loginRes.ExpiresAt,
			SNRefreshToken: &loginRes.RefreshToken,
		}
		if apiKey != "" {
			patch.Token = &apiKey
			patch.RefreshToken = &apiKey
		}
		if in.Name != "" {
			patch.Name = &in.Name
		}
		if in.Note != "" {
			patch.Note = &in.Note
		}
		online := store.StatusOnline
		patch.Status = &online
		if err := im.ST.UpdateAccount(ctx, existing.ID, patch); err != nil {
			return ExchangeResult{Message: "更新失败: " + err.Error()}
		}
		return ExchangeResult{OK: true, ID: existing.ID, UID: tenant, Updated: true, Message: "日日新账号已存在，凭据已更新"}
	}

	acc := &store.Account{
		UID:            tenant,
		Name:           display,
		Note:           in.Note,
		Provider:       store.ProviderSenseNova,
		Token:          apiKey,
		RefreshToken:   apiKey,
		Credits:        -1,
		Status:         store.StatusOnline,
		SNUsername:     in.Username,
		SNPassword:     in.Password,
		SNToken:        loginRes.AccessToken,
		SNTokenExp:     loginRes.ExpiresAt,
		SNRefreshToken: loginRes.RefreshToken,
		Language:       "zh",
		Meta:           map[string]any{"source": "sensenova-login"},
		Enabled:        true,
	}
	if err := im.ST.CreateAccount(ctx, acc); err != nil {
		return ExchangeResult{Message: "入库失败: " + err.Error()}
	}
	msg := "日日新账号已添加"
	if apiKey == "" {
		msg += "（未能自动创建 API Key，首次调用时会再尝试）"
	}
	return ExchangeResult{OK: true, ID: acc.ID, UID: tenant, Message: msg}
}

// findExisting 先按供应商内的账号名找，再按 uid（tenant_id）找。
func (im *SenseNovaImporter) findExisting(ctx context.Context, tenant, username string) (*store.Account, error) {
	accounts, err := im.ST.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var byUID *store.Account
	for _, a := range accounts {
		if !a.IsSenseNova() {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(a.SNUsername), username) {
			return a, nil
		}
		if byUID == nil && tenant != "" && a.UID == tenant {
			byUID = a
		}
	}
	return byUID, nil
}
