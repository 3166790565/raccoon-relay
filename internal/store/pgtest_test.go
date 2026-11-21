package store

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// openStoreAnyDialect 打开测试用存储。
//
// 默认走 SQLite（t.TempDir），现有测试无需改动。
// 设置 TEST_DATABASE_URL 时走 Postgres，每个测试独立 schema，
// 这样同一套用例能在两种方言下都跑一遍。
func openStoreAnyDialect(t *testing.T, secret string, allowPlaintext bool) *Store {
	t.Helper()
	if dsn := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL")); dsn != "" {
		return openPGTestStore(t, dsn, secret, allowPlaintext)
	}
	st, err := Open(t.TempDir(), secret, allowPlaintext)
	if err != nil {
		t.Fatalf("打开 SQLite 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// openPGTestStore 为单个测试建独立 schema，结束后整体 DROP。
// 不能共用一个 schema 清表：并发测试会互相干扰。
func openPGTestStore(t *testing.T, baseDSN, secret string, allowPlaintext bool) *Store {
	t.Helper()
	schema := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]

	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("连接 Postgres 失败: %v", err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		admin.Close()
	})

	st, err := OpenWithOptions(Options{
		Dir:            t.TempDir(),
		SecretKey:      secret,
		AllowPlaintext: allowPlaintext,
		DatabaseURL:    withSearchPath(baseDSN, schema),
		MaxOpen:        5,
		MaxIdle:        2,
	})
	if err != nil {
		t.Fatalf("打开 Postgres 测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// withSearchPath 把 search_path 追加到 DSN。
func withSearchPath(dsn, schema string) string {
	if strings.Contains(dsn, "search_path=") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}
