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
	Enabled      *bool          `json:"enabled,omitempty"`
	CreatedAt    int64          `json:"createdAt"`
	UpdatedAt    int64          `json:"updatedAt"`
	Meta         map[string]any `json:"meta"`

	// 日日新账号才有的控制台凭据。channel 为 sensenova 时 token 是 API Key，
	// 另有控制台账号密码与登录票，缺了它们只能靠 Key 用，过期后无法自动重登。
	Username       string `json:"username,omitempty"`
	Password       string `json:"password,omitempty"`
	ConsoleToken   string `json:"consoleToken,omitempty"`
	ConsoleExp     int64  `json:"consoleExp,omitempty"`
	ConsoleRefresh string `json:"consoleRefreshToken,omitempty"`
}

const (
	FormatCredentials = "agenthub-credentials"
	FormatMetadata    = "agenthub-accounts"
	ChannelRaccoon    = "raccoon"
	ChannelSenseNova  = "sensenova"
)

// ImportResult 逐条回传结果，失败不影响其它条。
type ImportResult struct {
	OK      bool   `json:"ok"`
	UID     string `json:"uid,omitempty"`
	ID      string `json:"id,omitempty"`
	Updated bool   `json:"updated,omitempty"`
	Message string `json:"message,omitempty"`
}

// ParseReport 记录一次解析里被过滤掉的条目，让面板能告诉用户"为什么只导入了这些"。
type ParseReport struct {
	Skipped int `json:"skipped"`
	// Providers 是跳过的非小浣熊供应商及条数，例如 {"qoder": 2, "trae": 1}。
	Providers map[string]int `json:"providers,omitempty"`
	// Invalid 是小浣熊账号里凭据不全、无法入库的条目说明。
	Invalid []string `json:"invalid,omitempty"`
}

// ParseImport 接受这几种形态：单个账号对象、账号数组、{channel,accounts:[]}，
// 以及 agent2api 的桌面导出 {version,exportedAt,accounts:[{provider,...}]}
// （后者是多供应商混装，只保留小浣熊账号）。
func ParseImport(raw []byte) ([]Account, error) {
	list, _, err := ParseImportWithReport(raw)
	return list, err
}

func ParseImportWithReport(raw []byte) ([]Account, *ParseReport, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil, fmt.Errorf("内容为空")
	}

	var probe map[string]any
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal(raw, &probe); err == nil {
			if f, _ := probe["format"].(string); f == FormatMetadata {
				return nil, nil, fmt.Errorf("这是账号元数据包（无 token），不能用于导入凭据")
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
	// 单个账号对象：先看是不是 agent2api 形态（token 在 pluginSecret 里）
	var asObj map[string]any
	if err := json.Unmarshal(raw, &asObj); err == nil && isAgent2APIAccount(asObj) {
		return decodeList([]any{asObj})
	}
	var one Account
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, nil, fmt.Errorf("无法解析导入内容: %w", err)
	}
	return []Account{one}, nil, nil
}

func decodeList(arr []any) ([]Account, *ParseReport, error) {
	out := make([]Account, 0, len(arr))
	report := &ParseReport{Providers: map[string]int{}}
	for i, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("第 %d 条不是对象", i+1)
		}
		if isAgent2APIAccount(obj) {
			// agent2api 导出是多供应商混装：只留小浣熊，其余计入报告后丢弃
			if !isRaccoonAgent2API(obj) {
				report.Skipped++
				report.Providers[agent2apiProvider(obj)]++
				continue
			}
			a, err := fromAgent2API(obj)
			if err != nil {
				// 单个坏条目不该让整份导出失败；记进报告，其余照常导入
				report.Skipped++
				report.Invalid = append(report.Invalid,
					fmt.Sprintf("第 %d 条（%s）: %v", i+1, strOf(obj["name"]), err))
				continue
			}
			out = append(out, a)
			continue
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, nil, fmt.Errorf("第 %d 条序列化失败: %w", i+1, err)
		}
		var a Account
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, nil, fmt.Errorf("第 %d 条格式非法: %w", i+1, err)
		}
		out = append(out, a)
	}
	if len(report.Providers) == 0 {
		report.Providers = nil
	}
	return out, report, nil
}

// isAgent2APIAccount 判断一条记录是不是 agent2api 桌面导出里的账号：
// token 放在 pluginSecret 里（桌面版凭据包没有这个字段），provider 可有可无。
func isAgent2APIAccount(v any) bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, hasSecret := obj["pluginSecret"].(map[string]any)
	return hasSecret
}

func agent2apiProvider(obj map[string]any) string {
	p, _ := obj["provider"].(string)
	if p == "" {
		return "unknown"
	}
	return p
}

// isRaccoonAgent2API 判别一条 agent2api 记录是否属于小浣熊：
// 优先看 provider 字段；provider 缺失时退回 token 的 iss。
func isRaccoonAgent2API(obj map[string]any) bool {
	if p := agent2apiProvider(obj); p != "unknown" {
		return p == ChannelRaccoon
	}
	tok := agent2apiToken(obj)
	if tok == "" {
		return false
	}
	c, err := raccoon.DecodeJWT(tok)
	return err == nil && strings.TrimSpace(c.ISS) == raccoon.Issuer
}

func agent2apiToken(obj map[string]any) string {
	if sec, ok := obj["pluginSecret"].(map[string]any); ok {
		if t, ok := sec["accessToken"].(string); ok && strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t)
		}
	}
	if t, ok := obj["accessToken"].(string); ok && strings.TrimSpace(t) != "" {
		return strings.TrimSpace(t)
	}
	return ""
}

// fromAgent2API 把一条 agent2api 记录映射成内部 Account。
// agent2api 把 token 放在 pluginSecret，meta 是扁平的点号键（pluginLedger.onboarding 等）。
func fromAgent2API(obj map[string]any) (Account, error) {
	a := Account{
		Channel: ChannelRaccoon,
		Name:    strOf(obj["name"]),
		Token:   agent2apiToken(obj),
		Source:  "agent2api",
		// agent2api 导出不含余额：-1 表示未知，否则 0 会被号池当成"零积分已确认"跳过。
		Credits: -1,
		Meta:    map[string]any{},
	}
	// uid 以 token 的 sub 为准：桌面导出与 OAuth 都是按它去重的，agent2api 里的
	// 数字 uid 是平台 ID，混用会让同一个号被当成两个号重复入库、各自签到消费。
	if tok := a.Token; tok != "" {
		if uid := raccoon.TokenUID(tok); uid != "" {
			a.UID = uid
		} else if c, err := raccoon.DecodeJWT(tok); err == nil {
			if s := strings.TrimSpace(c.Sub); s != "" {
				a.UID = s
			}
		}
	}
	if a.UID == "" {
		a.UID = strOf(obj["uid"])
	}
	if a.UID == "" {
		if pub, ok := obj["pluginPublic"].(map[string]any); ok {
			a.UID = strOf(pub["uid"])
		}
	}
	// 保留 agent2api 的平台 uid 方便排障
	if pu := strOf(obj["uid"]); pu != "" && pu != a.UID {
		a.Meta["platformUid"] = pu
	}
	if a.Name == "" {
		if pub, ok := obj["pluginPublic"].(map[string]any); ok {
			a.Name = strOf(pub["name"])
		}
	}
	if sec, ok := obj["pluginSecret"].(map[string]any); ok {
		if exp, ok := numOf(sec["expiresAt"]); ok {
			a.TokenExp = exp
		}
		if oi := strOf(sec["officeIdentity"]); oi != "" {
			a.Meta["officeIdentity"] = oi
		}
	}
	// 小浣熊的 refresh_token 与 access_token 是同一枚凭据（桌面导出实测两者逐字相同，
	// 刷新端点也只认这一枚）。agent2api 导出里没有独立的 refresh_token 字段，
	// 直接用 accessToken 回填，导入的号才能跟其它来源一样自动续期。
	a.RefreshToken = a.Token
	if enabled, ok := obj["enabled"].(bool); ok {
		a.Enabled = &enabled
	}
	if pr, ok := numOf(obj["priority"]); ok {
		a.Meta["priority"] = pr
	}
	// 把 agent2api 的扁平台账键收进 meta，去掉点号前缀便于面板/排障阅读。
	for k, v := range obj {
		if strings.HasPrefix(k, "pluginLedger.") {
			a.Meta[strings.TrimPrefix(k, "pluginLedger.")] = v
		}
		if k == "checkinAt" || k == "addedAt" || k == "updatedAt" {
			a.Meta[k] = v
		}
	}
	if a.Token == "" {
		return a, fmt.Errorf("缺少 accessToken")
	}
	if a.UID == "" {
		a.UID = raccoon.TokenUID(a.Token)
	}
	if a.UID == "" {
		return a, fmt.Errorf("无法从 token 解析账号 uid")
	}
	return a, nil
}

func strOf(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func numOf(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
	}
	return 0, false
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
	if a.Channel == ChannelSenseNova {
		return im.importSenseNova(ctx, a)
	}
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
			Token:          &token,
			RefreshToken:   &a.RefreshToken,
			TokenExp:       &tokenExp,
			OfficeIdentity: &officeIdentity,
			Meta:           merged,
		}
		// 重新导入只更新凭据，不拿导入包里的「未知」值覆盖已知状态：
		// agent2api 的 Credits 恒为 -1、ExpiresAt 恒为 0，无条件写会把面板上
		// 真实的余额和到期时间清掉（余额变成 -1 后负载均衡还会把它当积分最多优先选）。
		if a.Credits >= 0 {
			p.Credits = &a.Credits
		}
		if a.ExpiresAt > 0 {
			p.ExpiresAt = &a.ExpiresAt
		}
		if name != "" {
			p.Name = &name
		}
		if a.Note != "" {
			p.Note = &a.Note
		}
		// 导入包明确给出状态时才改状态；否则保留现有的 cooling/exhausted/relogin，
		// 一次重导入不该把正在冷却或积分耗尽的号放回池子。
		if strings.TrimSpace(a.Status) != "" {
			p.Status = &status
			if status == store.StatusOnline {
				zero := int64(0)
				empty := ""
				p.CoolUntil = &zero
				p.CoolReason = &empty
			}
		}
		if err := im.ST.UpdateAccount(ctx, existing.ID, p); err != nil {
			return ImportResult{UID: uid, Message: "更新失败: " + err.Error()}
		}
		return ImportResult{OK: true, UID: uid, ID: existing.ID, Updated: true}
	}

	// 新号的指纹取当前生效值：面板里改过桌面平台/系统版本后，
	// 之后导入的号应当与正在跑的请求保持同一套客户端身份。
	fp := raccoon.CurrentFingerprint()
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
		OSVersion:      fp.OSVersion,
		OfficeIdentity: officeIdentity,
		Platform:       fp.DesktopPlatform,
		ClientChannel:  "stable",
		Language:       "zh",
		Meta:           meta,
		// agent2api 导出里明确标记了 enabled:false 的号保留禁用态，其余默认启用
		Enabled: a.Enabled == nil || *a.Enabled,
	}
	if err := im.ST.CreateAccount(ctx, acc); err != nil {
		return ImportResult{UID: uid, Message: "入库失败: " + err.Error()}
	}
	return ImportResult{OK: true, UID: uid, ID: acc.ID}
}

// importSenseNova 导入导出包里的日日新账号。
//
// 只写库、不联网：登录票过期由转发层用 refresh_token / 账号密码自动重登，
// 导入时没必要再打一次上游。按「供应商 + uid」去重；缺 uid 时用控制台账号兜底。
func (im *Importer) importSenseNova(ctx context.Context, a Account) ImportResult {
	username := strings.TrimSpace(a.Username)
	uid := strings.TrimSpace(a.UID)
	if uid == "" {
		uid = username
	}
	if uid == "" {
		return ImportResult{Message: "日日新账号缺少 uid 与控制台账号，已跳过"}
	}
	apiKey := strings.TrimSpace(a.Token)
	if apiKey == "" {
		return ImportResult{Message: "日日新账号缺少 API Key，已跳过"}
	}
	meta := a.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	if _, ok := meta["source"]; !ok {
		meta["source"] = "import"
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = a.Username
	}

	existing, err := im.ST.GetByProviderUID(ctx, store.ProviderSenseNova, uid)
	if err != nil {
		return ImportResult{UID: uid, Message: "查询失败: " + err.Error()}
	}
	if existing != nil {
		p := store.AccountPatch{
			Token:          &apiKey,
			RefreshToken:   &apiKey,
			Meta:           mergeMeta(existing.Meta, meta),
			SNUsername:     &a.Username,
			SNPassword:     &a.Password,
			SNToken:        &a.ConsoleToken,
			SNTokenExp:     &a.ConsoleExp,
			SNRefreshToken: &a.ConsoleRefresh,
		}
		if name != "" {
			p.Name = &name
		}
		if a.Note != "" {
			p.Note = &a.Note
		}
		// 导出包里 Credits 恒为 -1（未知），不能拿它覆盖库里的真实额度。
		if a.Credits >= 0 {
			p.Credits = &a.Credits
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
		Provider:       store.ProviderSenseNova,
		Token:          apiKey,
		RefreshToken:   apiKey,
		Credits:        -1,
		Status:         store.StatusOnline,
		SNUsername:     a.Username,
		SNPassword:     a.Password,
		SNToken:        a.ConsoleToken,
		SNTokenExp:     a.ConsoleExp,
		SNRefreshToken: a.ConsoleRefresh,
		Language:       "zh",
		Meta:           meta,
		Enabled:        a.Enabled == nil || *a.Enabled,
	}
	if a.Credits >= 0 {
		acc.Credits = a.Credits
	}
	if err := im.ST.CreateAccount(ctx, acc); err != nil {
		return ImportResult{UID: uid, Message: "入库失败: " + err.Error()}
	}
	return ImportResult{OK: true, UID: uid, ID: acc.ID}
}

// Export 读全池生成与桌面版同格式的 JSON。导出明文 token（与桌面版行为一致）。
//
// 两种供应商都导出：小浣熊行与桌面版逐字段对齐，日日新行额外带控制台账号密码
// 与登录票，导出结果能原样导入回来继续自动续期/重登。信封的 channel 保持
// raccoon 不变（老版本只认它），按需导出的供应商由 ids 里的实际条目决定。
func Export(ctx context.Context, st *store.Store) (*File, error) {
	return ExportIDs(ctx, st, nil)
}

// ExportIDs 只导出 ids 里的号；ids 为空表示全池。
func ExportIDs(ctx context.Context, st *store.Store, ids []string) (*File, error) {
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	all := len(want) == 0
	f := &File{
		Format:     FormatCredentials,
		ExportedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Channel:    ChannelRaccoon,
		Accounts:   make([]Account, 0, len(accounts)),
	}
	for _, a := range accounts {
		if !all && !want[a.ID] {
			continue
		}
		f.Accounts = append(f.Accounts, exportOne(a))
	}
	return f, nil
}

func exportOne(a *store.Account) Account {
	out := Account{
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
	}
	if a.IsSenseNova() {
		// 日日新的 Token 是 API Key（没有 exp），有效期看控制台登录票。
		out.Channel = ChannelSenseNova
		out.Username = a.SNUsername
		out.Password = a.SNPassword
		out.ConsoleToken = a.SNToken
		out.ConsoleExp = a.SNTokenExp
		out.ConsoleRefresh = a.SNRefreshToken
		out.TokenExp = 0
	}
	return out
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
