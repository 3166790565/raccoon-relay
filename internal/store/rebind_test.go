package store

import (
	"strings"
	"testing"
)

func TestRebindPostgres(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"无占位符", `SELECT * FROM accounts`, `SELECT * FROM accounts`},
		{"单个", `SELECT id FROM api_keys WHERE hash = ?`, `SELECT id FROM api_keys WHERE hash = $1`},
		{"多个", `INSERT INTO t (a,b) VALUES (?,?)`, `INSERT INTO t (a,b) VALUES ($1,$2)`},
		{"IN 列表", `DELETE FROM t WHERE id IN (SELECT id FROM t WHERE ts < ? ORDER BY id LIMIT ?)`,
			`DELETE FROM t WHERE id IN (SELECT id FROM t WHERE ts < $1 ORDER BY id LIMIT $2)`},
		{"单引号内含问号", `SELECT '?' || id FROM t WHERE a = ?`, `SELECT '?' || id FROM t WHERE a = $1`},
		{"单引号转义", `SELECT 'it''s ?' FROM t WHERE a = ?`, `SELECT 'it''s ?' FROM t WHERE a = $1`},
		{"双引号标识符", `SELECT "count?" FROM t WHERE a = ?`, `SELECT "count?" FROM t WHERE a = $1`},
		{"反引号", "SELECT `a?` FROM t WHERE b = ?", "SELECT `a?` FROM t WHERE b = $1"},
		{"行注释", "SELECT a FROM t -- 这是 ?\nWHERE b = ?", "SELECT a FROM t -- 这是 ?\nWHERE b = $1"},
		{"块注释", `SELECT a /* ? */ FROM t WHERE b = ?`, `SELECT a /* ? */ FROM t WHERE b = $1`},
		{"dollar quote", `SELECT $$?$$ FROM t WHERE a = ?`, `SELECT $$?$$ FROM t WHERE a = $1`},
		{"tagged dollar quote", `SELECT $tag$?$tag$ FROM t WHERE a = ?`, `SELECT $tag$?$tag$ FROM t WHERE a = $1`},
		{"未闭合引号", `SELECT 'abc`, `SELECT 'abc`},
		{"区间与位数", `SELECT ? , ? , ? , ? , ? , ? , ? , ? , ? , ? , ?`,
			`SELECT $1 , $2 , $3 , $4 , $5 , $6 , $7 , $8 , $9 , $10 , $11`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rebindPostgres(tc.in); got != tc.want {
				t.Fatalf("rebind 结果不符\n输入: %s\n期望: %s\n实际: %s", tc.in, tc.want, got)
			}
		})
	}
}

// SQLite 下 rebind 必须是恒等函数：一个字符都不能改。
func TestRebindSQLiteIsIdentity(t *testing.T) {
	in := `SELECT '?' , "x?" , a FROM t WHERE b = ? -- ?`
	if got := dialectSQLite.rebind(in); got != in {
		t.Fatalf("SQLite 方言不应改写 SQL\n期望: %s\n实际: %s", in, got)
	}
}

// 占位符数量应等于参数个数。这条测试用于固定当前 SQL 的假设：
// 如果有人新写 SQL 时在字符串字面量里放了 ?，这里不会失败，
// 但 rebind 的单元测试会覆盖到；这里额外保证整体计数一致。
func TestRebindPlaceholderCountMatchesArgs(t *testing.T) {
	cases := []struct {
		sql  string
		args int
	}{
		{`INSERT INTO api_keys (id, name, prefix, hash, enabled, created_at, last_used) VALUES (?,?,?,?,1,?,0)`, 5},
		{`UPDATE runtime_events SET count = count + ?, last_ts = ?, detail = ?
		  WHERE id = (SELECT id FROM runtime_events
			WHERE level = ? AND scope = ? AND component = ? AND account_id = ?
			  AND message = ? AND ts >= ?
			ORDER BY id DESC LIMIT 1)`, 9},
	}
	for _, tc := range cases {
		got := strings.Count(tc.sql, "?")
		if got != tc.args {
			t.Fatalf("占位符数量不符: 期望 %d，实际 %d\nSQL: %s", tc.args, got, tc.sql)
		}
	}
}
