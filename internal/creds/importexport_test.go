package creds

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"raccoon-relay/internal/store"
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
