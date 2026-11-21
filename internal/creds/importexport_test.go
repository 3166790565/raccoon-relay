package creds

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, "test-secret", false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 用桌面导出的真实样例做往返测试：导入 → 导出 → 关键字段一致。
func TestRoundTripRealDesktopFile(t *testing.T) {
	src := filepath.Join("testdata", "agenthub-credentials-raccoon.json")
	if _, err := os.Stat(src); err != nil {
		t.Skip("缺少样例文件，跳过")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	list, err := ParseImport(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(list) != 5 {
		t.Fatalf("期望 5 条，实际 %d", len(list))
	}

	st := newTestStore(t)
	im := &Importer{ST: st}
	ctx := context.Background()
	results := im.Import(ctx, list)
	for _, r := range results {
		if !r.OK {
			t.Fatalf("导入失败: %+v", r)
		}
	}

	out, err := Export(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if out.Format != FormatCredentials || out.Channel != ChannelRaccoon {
		t.Fatalf("信封字段不对: %s/%s", out.Format, out.Channel)
	}
	if len(out.Accounts) != 5 {
		t.Fatalf("导出条数不对: %d", len(out.Accounts))
	}

	byUID := map[string]Account{}
	for _, a := range out.Accounts {
		byUID[a.UID] = a
	}
	for _, want := range list {
		got, ok := byUID[want.UID]
		if !ok {
			t.Fatalf("导出缺少 uid %s", want.UID)
		}
		if got.Token != want.Token {
			t.Fatalf("token 往返不一致: %s", want.UID)
		}
		if got.RefreshToken != want.RefreshToken {
			t.Fatalf("refreshToken 往返不一致: %s", want.UID)
		}
		if got.TokenExp != want.TokenExp {
			t.Fatalf("tokenExp 往返不一致: %d vs %d", got.TokenExp, want.TokenExp)
		}
		if got.Credits != want.Credits {
			t.Fatalf("credits 往返不一致: %d vs %d", got.Credits, want.Credits)
		}
		// meta 里的台账与设备指纹必须保住
		gotCheckin, _ := got.Meta["checkin"].(map[string]any)
		wantCheckin, _ := want.Meta["checkin"].(map[string]any)
		if gotCheckin["day"] != wantCheckin["day"] {
			t.Fatalf("meta.checkin.day 丢失: %v vs %v", gotCheckin["day"], wantCheckin["day"])
		}
		if got.Meta["deviceId"] != want.Meta["deviceId"] {
			t.Fatalf("meta.deviceId 被改动: %v vs %v", got.Meta["deviceId"], want.Meta["deviceId"])
		}
	}
}

// 同一份文件导入两次：按 uid 去重，第二次全部是 updated。
func TestImportDedupesByUID(t *testing.T) {
	src := filepath.Join("testdata", "agenthub-credentials-raccoon.json")
	if _, err := os.Stat(src); err != nil {
		t.Skip("缺少样例文件，跳过")
	}
	raw, _ := os.ReadFile(src)
	list, err := ParseImport(raw)
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	im := &Importer{ST: st}
	ctx := context.Background()

	first := im.Import(ctx, list)
	for _, r := range first {
		if r.Updated {
			t.Fatal("首次导入不应标记 updated")
		}
	}
	second := im.Import(ctx, list)
	for _, r := range second {
		if !r.Updated {
			t.Fatalf("二次导入应全部 updated: %+v", r)
		}
	}
	accounts, _ := st.ListAccounts(ctx)
	if len(accounts) != len(list) {
		t.Fatalf("去重失败：库里 %d 条，源 %d 条", len(accounts), len(list))
	}
}

// agent2api 多供应商导出：只保留 raccoon 账号，其余计入跳过报告。
func TestParseImportAgent2APIFiltersOtherProviders(t *testing.T) {
	raw, err := readAgent2APISample()
	if err != nil {
		t.Skip("缺少样例文件，跳过")
	}
	list, report, err := ParseImportWithReport(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("期望保留 2 个小浣熊账号，实际 %d", len(list))
	}
	if report == nil || report.Skipped != 2 {
		t.Fatalf("跳过报告不对: %+v", report)
	}
	if report.Providers["qoder"] != 1 || report.Providers["trae"] != 1 {
		t.Fatalf("供应商统计不对: %+v", report.Providers)
	}
	byUID := map[string]Account{}
	for _, a := range list {
		byUID[a.UID] = a
	}
	// uid 以 token 的 sub 为准（与桌面导出/OAuth 同口径，避免同一个号被重复入库）；
	// 导出里的数字 uid 保留在 meta.platformUid 供排障。
	a, ok := byUID["pu-aaaa"]
	if !ok {
		t.Fatalf("缺少账号 pu-aaaa: %+v", byUID)
	}
	if a.Meta["platformUid"] != "7991512" {
		t.Fatalf("数字 uid 应保留在 meta.platformUid: %+v", a.Meta["platformUid"])
	}
	if a.Token == "" || a.Name != "RaccoonSophia" {
		t.Fatalf("字段映射不对: %+v", a)
	}
	if a.TokenExp != 1797086942000 {
		t.Fatalf("tokenExp 应取 pluginSecret.expiresAt，实际 %d", a.TokenExp)
	}
	if a.Credits != -1 {
		t.Fatalf("agent2api 无余额，应记为未知 -1，实际 %d", a.Credits)
	}
	if a.Meta["officeIdentity"] != "personal" {
		t.Fatalf("officeIdentity 未进 meta: %+v", a.Meta)
	}
	if _, ok := a.Meta["onboardingGrants"]; !ok {
		t.Fatalf("首登奖台账未进 meta: %+v", a.Meta)
	}
	if a.Enabled == nil || !*a.Enabled {
		t.Fatalf("enabled:true 应保留启用态: %+v", a.Enabled)
	}
	if b := byUID["pu-bbbb"]; b.Enabled == nil || *b.Enabled {
		t.Fatalf("enabled:false 应保留禁用态: %+v", b.Enabled)
	}
}

// provider 缺失时用 token 的 iss 判别归属，别把别的供应商 token 当小浣熊收进来。
func TestAgent2APIProviderFallbackToIssuer(t *testing.T) {
	raw := `{"accounts":[
		{"name":"r","pluginSecret":{"accessToken":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwdS14IiwiaXNzIjoieGlhb2h1YW54aW9uZy5jb20vaWFtIn0.s"}},
		{"name":"x","pluginSecret":{"accessToken":"opaque-token"}}
	]}`
	list, report, err := ParseImportWithReport([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].UID != "pu-x" {
		t.Fatalf("iss 兜底判别失败: %+v", list)
	}
	if report.Skipped != 1 {
		t.Fatalf("应跳过 1 条: %+v", report)
	}
}

// 导入后走完整入库：enabled 状态落库，权重等既有字段不被 agent2api 覆盖。
func TestImportAgent2APIAccounts(t *testing.T) {
	raw, err := readAgent2APISample()
	if err != nil {
		t.Skip("缺少样例文件，跳过")
	}
	list, _, err := ParseImportWithReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	im := &Importer{ST: st}
	ctx := context.Background()
	for _, r := range im.Import(ctx, list) {
		if !r.OK {
			t.Fatalf("入库失败: %+v", r)
		}
	}
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("库里应有 2 条，实际 %d", len(accounts))
	}
	byUID := map[string]*store.Account{}
	for _, a := range accounts {
		byUID[a.UID] = a
	}
	if !byUID["pu-aaaa"].Enabled || byUID["pu-bbbb"].Enabled {
		t.Fatalf("enabled 落库不对")
	}
	if byUID["pu-aaaa"].Credits != -1 {
		t.Fatalf("未知余额应保持 -1，实际 %d", byUID["pu-aaaa"].Credits)
	}
}

func TestParseImportRejectsMetadataOnly(t *testing.T) {
	raw := `{"format":"agenthub-accounts","accounts":[{"uid":"pu-x"}]}`
	if _, err := ParseImport([]byte(raw)); err == nil {
		t.Fatal("元数据包应被拒绝并给出明确错误")
	}
}

func TestParseImportAcceptsArrayAndSingle(t *testing.T) {
	arr := `[{"uid":"pu-1","token":"a"},{"uid":"pu-2","token":"b"}]`
	if list, err := ParseImport([]byte(arr)); err != nil || len(list) != 2 {
		t.Fatalf("数组形态解析失败: %v %d", err, len(list))
	}
	single := `{"uid":"pu-3","token":"c"}`
	if list, err := ParseImport([]byte(single)); err != nil || len(list) != 1 {
		t.Fatalf("单对象形态解析失败: %v %d", err, len(list))
	}
}

func TestImportSkipsMissingToken(t *testing.T) {
	st := newTestStore(t)
	im := &Importer{ST: st}
	res := im.Import(context.Background(), []Account{{UID: "pu-x"}})
	if res[0].OK {
		t.Fatal("缺 token 应跳过")
	}
}

// 导出必须能直接回灌：把导出结果再解析一次，条数一致。
func TestExportReimportable(t *testing.T) {
	st := newTestStore(t)
	im := &Importer{ST: st}
	ctx := context.Background()
	im.Import(ctx, []Account{{
		UID: "pu-1", Token: "tok", RefreshToken: "ref", Credits: 100, Status: "online",
		Meta: map[string]any{"deviceId": "dev-1"},
	}})

	f, err := Export(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f)
	list, err := ParseImport(raw)
	if err != nil || len(list) != 1 {
		t.Fatalf("导出结果无法回灌: %v", err)
	}
	if list[0].UID != "pu-1" || list[0].Token != "tok" {
		t.Fatalf("回灌字段错乱: %+v", list[0])
	}
}

// 日日新账号也要导出成完整的凭据包：API Key 之外还要带控制台账号密码与
// 登录票，否则导出后再导入只能靠 Key 用，票过期就没法自动重登。
func TestExportIncludesSenseNovaCredentials(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sn := &store.Account{
		UID: "tenant-9", Name: "日日新号", Provider: store.ProviderSenseNova,
		Token: "sk-key", RefreshToken: "sk-key", Credits: 42,
		SNUsername: "u@example.com", SNPassword: "pw",
		SNToken: "console-jwt", SNTokenExp: 1700000000000, SNRefreshToken: "console-refresh",
		Enabled: true,
	}
	if err := st.CreateAccount(ctx, sn); err != nil {
		t.Fatal(err)
	}

	f, err := Export(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 {
		t.Fatalf("导出条数不对: %d", len(f.Accounts))
	}
	got := f.Accounts[0]
	if got.Channel != ChannelSenseNova {
		t.Fatalf("channel=%q，应为 sensenova", got.Channel)
	}
	if got.Token != "sk-key" || got.Username != "u@example.com" || got.Password != "pw" ||
		got.ConsoleToken != "console-jwt" || got.ConsoleExp != 1700000000000 ||
		got.ConsoleRefresh != "console-refresh" {
		t.Fatalf("日日新凭据字段不完整: %+v", got)
	}

	// 回灌：清空后重新导入应能建回同一个号。
	if err := st.DeleteAccount(ctx, sn.ID); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f)
	list, err := ParseImport(raw)
	if err != nil || len(list) != 1 {
		t.Fatalf("日日新导出无法回灌: %v %d", err, len(list))
	}
	res := (&Importer{ST: st}).Import(ctx, list)
	if !res[0].OK {
		t.Fatalf("日日新回灌失败: %+v", res[0])
	}
	back, err := st.GetByProviderUID(ctx, store.ProviderSenseNova, "tenant-9")
	if err != nil || back == nil {
		t.Fatalf("回灌后没找到账号: %v", err)
	}
	if back.SNUsername != "u@example.com" || back.SNPassword != "pw" || back.Token != "sk-key" {
		t.Fatalf("回灌凭据丢失: %+v", back)
	}
	if back.Credits != 42 {
		t.Fatalf("回灌后余额=%d，应为 42", back.Credits)
	}
}

// 只导勾选的号：ids 之外的账号不出现在包里，其余账号导出行为不变。
func TestExportIDsFiltersSelection(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	im := &Importer{ST: st}
	im.Import(ctx, []Account{
		{UID: "pu-1", Token: "t1", Meta: map[string]any{}},
		{UID: "pu-2", Token: "t2", Meta: map[string]any{}},
	})
	accounts, err := st.ListAccounts(ctx)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("准备数据失败: %v %d", err, len(accounts))
	}
	var target string
	for _, a := range accounts {
		if a.UID == "pu-2" {
			target = a.ID
		}
	}
	f, err := ExportIDs(ctx, st, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].UID != "pu-2" {
		t.Fatalf("勾选导出结果不对: %+v", f.Accounts)
	}
	// 空 ids 等同全池
	all, err := ExportIDs(ctx, st, nil)
	if err != nil || len(all.Accounts) != 2 {
		t.Fatalf("空 ids 应导出全池: %v %d", err, len(all.Accounts))
	}
}

// agent2api 的 refresh_token 与 access_token 是同一枚凭据，导入时应直接用
// pluginSecret.accessToken 回填 refreshToken，这样 30 天后还能自动续期。
func TestImportAgent2APIFillsRefreshToken(t *testing.T) {
	raw, err := readAgent2APISample()
	if err != nil {
		t.Skip("缺少样例文件，跳过")
	}
	list, _, err := ParseImportWithReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.RefreshToken != a.Token {
			t.Fatalf("%s 的 refreshToken 应等于 token，实际 %q vs %q", a.UID, a.RefreshToken, a.Token)
		}
	}
}

// readAgent2APISample 读 agent2api 样例。该文件含真实 token，被 .gitignore 排除，
// CI 上没有它，调用方要按「缺少样例则跳过」处理。
func readAgent2APISample() ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", "agent2api-mixed.json"))
}
