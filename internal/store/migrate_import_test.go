package store

import (
	"context"
	"testing"
)

// 在没有 Postgres 的环境里验证迁移的核心逻辑：
// 读源库 → 校验密文 → 写入目标库（这里目标库也用 SQLite，走同一套包装方法）
// → 逐表行数比对。
//
// setval 只对 Postgres 有意义，因此这里直接调用 importDump 的公共前置与
// 校验部分，不触发序列重置。
func TestImportDumpAndVerifyCounts(t *testing.T) {
	ctx := context.Background()

	srcDir := t.TempDir()
	src, err := Open(srcDir, "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.CreateAccount(ctx, &Account{UID: "u1", Name: "甲", Token: "tok1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := src.CreateAccount(ctx, &Account{UID: "u2", Name: "乙", Token: "tok2", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.CreateKey(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := src.AddUsage(ctx, UsageLog{Model: "m", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	if err := src.SetSetting(ctx, "strategy", "round_robin"); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	importing, err := checkpointAndRename(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restoreDumpFiles(importing) })

	dump, err := readSQLiteDump(importing)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCipher("test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDumpCipher(c, dump); err != nil {
		t.Fatal(err)
	}

	// 目标库：新建一个空 SQLite，用同一事务把数据写进去。
	dst, err := Open(t.TempDir(), "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	tx, err := dst.beginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.importDumpForTest(ctx, tx, dump); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := dst.verifyTargetCounts(ctx, tx, dump); err != nil {
		_ = tx.Rollback()
		t.Fatalf("行数比对失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	accs, err := dst.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 2 {
		t.Fatalf("导入后应有 2 个账号，实际 %d", len(accs))
	}
	for _, a := range accs {
		if a.Token != "tok1" && a.Token != "tok2" {
			t.Fatalf("密文复制后应能用同一 SECRET_KEY 解开，实际 %q", a.Token)
		}
	}
}
