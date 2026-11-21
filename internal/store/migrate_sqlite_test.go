package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// 旧库读出应等价于写入的内容，密文列保持密文。
func TestReadSQLiteDumpRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, err := Open(dir, "test-key", false)
	if err != nil {
		t.Fatalf("建源库失败: %v", err)
	}
	if err := src.CreateAccount(ctx, &Account{UID: "u1", Name: "甲", Token: "tok", Enabled: true}); err != nil {
		t.Fatalf("写账号失败: %v", err)
	}
	if _, _, err := src.CreateKey(ctx, "k1"); err != nil {
		t.Fatalf("写 Key 失败: %v", err)
	}
	if err := src.AddUsage(ctx, UsageLog{Model: "m1", Status: 200}); err != nil {
		t.Fatalf("写流水失败: %v", err)
	}
	if err := src.SetSetting(ctx, "strategy", "round_robin"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	_ = src.Close()

	// 先把 WAL 合并进主库，再按迁移流程改名读取。
	importing, err := checkpointAndRename(dir)
	if err != nil {
		t.Fatalf("checkpoint/改名失败: %v", err)
	}
	t.Cleanup(func() { restoreDumpFiles(importing) })

	dump, err := readSQLiteDump(importing)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(dump.Accounts) != 1 || dump.Accounts[0].Name != "甲" {
		t.Fatalf("账号读取不符: %+v", dump.Accounts)
	}
	// token 应以密文形式读出，不能是明文。
	if dump.Accounts[0].Token == "tok" || dump.Accounts[0].Token == "" {
		t.Fatalf("token 应以密文读出，实际: %q", dump.Accounts[0].Token)
	}
	if len(dump.Keys) != 1 || len(dump.Usage) != 1 {
		t.Fatalf("Key/流水读取不符: keys=%d usage=%d", len(dump.Keys), len(dump.Usage))
	}
	if len(dump.Settings) != 1 || dump.Settings[0].Key != "strategy" {
		t.Fatalf("设置读取不符: %+v", dump.Settings)
	}

	c, err := NewCipher("test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDumpCipher(c, dump); err != nil {
		t.Fatalf("同一 SECRET_KEY 应能校验通过: %v", err)
	}
}

// SECRET_KEY 不一致时必须在导入前就失败，而不是导入一个打不开的库。
func TestVerifyDumpCipherRejectsWrongKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, err := Open(dir, "key-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.CreateAccount(ctx, &Account{UID: "u1", Token: "tok", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	importing, err := checkpointAndRename(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restoreDumpFiles(importing) })

	dump, err := readSQLiteDump(importing)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := NewCipher("key-b", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDumpCipher(wrong, dump); err == nil {
		t.Fatal("SECRET_KEY 不一致时应报错")
	}
}

// 旧版本可能缺后来新增的表/列（例如 account_cooldowns、checkin_history.source）。
// 迁移应把它们读成空值，而不是整体失败。
func TestReadDumpToleratesOlderSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")

	// 手工建一个「老版本」库：没有 account_cooldowns，checkin_history 没有 source。
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE accounts (id TEXT PRIMARY KEY, uid TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '', token TEXT NOT NULL DEFAULT '', refresh_token TEXT NOT NULL DEFAULT '',
			token_exp INTEGER NOT NULL DEFAULT 0, credits INTEGER NOT NULL DEFAULT -1,
			credits_at INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'online',
			cool_until INTEGER NOT NULL DEFAULT 0, cool_reason TEXT NOT NULL DEFAULT '',
			expires_at INTEGER NOT NULL DEFAULT 0, device_id TEXT NOT NULL DEFAULT '',
			device_name TEXT NOT NULL DEFAULT '', os_version TEXT NOT NULL DEFAULT '',
			office_identity TEXT NOT NULL DEFAULT '', platform TEXT NOT NULL DEFAULT '',
			client_channel TEXT NOT NULL DEFAULT '', language TEXT NOT NULL DEFAULT 'zh',
			meta TEXT NOT NULL DEFAULT '{}', priority INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1, last_checkin_day TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0,
			last_used INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE api_keys (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '',
			prefix TEXT NOT NULL DEFAULT '', hash TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT 0, last_used INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE checkin_history (id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL, day TEXT NOT NULL, action TEXT NOT NULL DEFAULT '',
			ok INTEGER NOT NULL DEFAULT 0, already INTEGER NOT NULL DEFAULT 0,
			credit INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT '',
			at INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO accounts (id, uid, token) VALUES ('a1', 'u1', 'enc:plain:tok')`,
		`INSERT INTO checkin_history (account_id, day, action, ok) VALUES ('a1', '2026-01-01', 'daily', 1)`,
	}
	for _, q := range stmts {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("建旧库失败: %v\nSQL: %s", err, q)
		}
	}
	_ = raw.Close()

	dump, err := readSQLiteDump(dbPath)
	if err != nil {
		t.Fatalf("旧 schema 不应导致读取失败: %v", err)
	}
	if len(dump.Accounts) != 1 || len(dump.Checkins) != 1 {
		t.Fatalf("账号/签到读取不符: %d / %d", len(dump.Accounts), len(dump.Checkins))
	}
	if dump.Checkins[0].Source != "" {
		t.Fatalf("缺失的 source 列应为空串，实际 %q", dump.Checkins[0].Source)
	}
	if dump.AccountCools != nil {
		t.Fatalf("缺失的 account_cooldowns 应为空集，实际 %d 行", len(dump.AccountCools))
	}
}

// 迁移失败改名回原位后，原库必须仍可正常打开。
func TestRestoreDumpFiles(t *testing.T) {
	dir := t.TempDir()
	src, err := Open(dir, "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	importing, err := checkpointAndRename(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "relay.db")); !os.IsNotExist(err) {
		t.Fatal("改名后原路径不应还能找到 relay.db")
	}
	restoreDumpFiles(importing)

	again, err := Open(dir, "test-key", false)
	if err != nil {
		t.Fatalf("还原后应能再次打开: %v", err)
	}
	_ = again.Close()
}

// 非 Postgres 的 Store 调用迁移应直接跳过，不碰任何文件。
func TestMigrateSQLiteSkipsOnSQLiteStore(t *testing.T) {
	st := openTestStore(t)
	if st.Dialect() != dialectSQLite {
		t.Skip("仅验证 SQLite 方言下的跳过行为")
	}
	res, err := st.MigrateSQLite(context.Background(), t.TempDir(), MigrateAuto)
	if err != nil {
		t.Fatalf("SQLite 上不应报错: %v", err)
	}
	if !res.Skipped || res.Ran {
		t.Fatalf("应跳过: %+v", res)
	}
}

func TestParseMigrateMode(t *testing.T) {
	cases := map[string]MigrateMode{
		"off": MigrateOff, "require": MigrateRequire,
		"auto": MigrateAuto, "": MigrateAuto, "乱写": MigrateAuto,
	}
	for in, want := range cases {
		if got := ParseMigrateMode(in); got != want {
			t.Fatalf("ParseMigrateMode(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// 以下用例需要真实 Postgres：TEST_DATABASE_URL 为空时跳过。
func TestMigrateSQLiteToPostgres(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("未设置 TEST_DATABASE_URL")
	}
	ctx := context.Background()

	// 造一份带数据的旧 SQLite 库。
	dir := t.TempDir()
	src, err := Open(dir, "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := src.CreateAccount(ctx, &Account{UID: "u" + string(rune('a'+i)), Token: "tok", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := src.CreateKey(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := src.AddUsage(ctx, UsageLog{Model: "m", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	_ = src.Close()

	dst := openStoreAnyDialect(t, "test-key", false)
	if dst.Dialect() != dialectPostgres {
		t.Skip("测试库不是 Postgres")
	}

	res, err := dst.MigrateSQLite(ctx, dir, MigrateAuto)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !res.Ran {
		t.Fatalf("应执行迁移: %+v", res)
	}
	if res.Counts["accounts"] != 3 || res.Counts["usage_logs"] != 20 {
		t.Fatalf("迁移计数不符: %+v", res.Counts)
	}

	// 数据可读、密文可解。
	accs, err := dst.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 3 || accs[0].Token != "tok" {
		t.Fatalf("账号迁移后不可用: %+v", accs)
	}

	// 序列已推进：新插入的流水不会撞主键。
	if err := dst.AddUsage(ctx, UsageLog{Model: "new", Status: 200}); err != nil {
		t.Fatalf("迁移后新增流水失败（序列未重置？）: %v", err)
	}

	// 再跑一次应跳过。
	again, err := dst.MigrateSQLite(ctx, dir, MigrateAuto)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ran {
		t.Fatalf("重复迁移应跳过: %+v", again)
	}
}