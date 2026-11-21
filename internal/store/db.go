package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Store 是所有持久化的入口。db 可能是 SQLite 也可能是 Postgres，
// 业务代码一律通过 exec/query/queryRow 访问，不直接碰 db。
type Store struct {
	db      *sql.DB
	cipher  *Cipher
	dialect dialect

	// 热路径缓存：Key 校验命中、last_used 写节流、账号列表快照。
	keys keyCacheState
	snap accountSnapshot
	// usage 异步写入器，见 usage_async.go。
	usageOnce     sync.Once
	usageStopOnce sync.Once
	usageQ        chan UsageLog
	usagePending  int64
}

// Options 是打开存储层的完整参数。零值等价于纯 SQLite 本地模式。
type Options struct {
	Dir            string
	SecretKey      string
	AllowPlaintext bool

	// DatabaseURL 为空时走 SQLite（$Dir/relay.db）；以 postgres:// 开头时走 Postgres。
	DatabaseURL string
	// MaxOpen / MaxIdle 为 0 时按方言取默认：SQLite 1/1，Postgres 20/10。
	MaxOpen int
	MaxIdle int
}

// Open 保留原有签名，始终使用 SQLite。测试与旧的单机部署都走这里。
func Open(dir, secret string, allowPlaintext bool) (*Store, error) {
	return OpenWithOptions(Options{Dir: dir, SecretKey: secret, AllowPlaintext: allowPlaintext})
}

// OpenWithOptions 按 Options 打开 SQLite 或 Postgres，并执行建表迁移。
func OpenWithOptions(o Options) (*Store, error) {
	c, err := NewCipher(o.SecretKey, o.AllowPlaintext)
	if err != nil {
		return nil, err
	}

	d := dialectSQLite
	driver, dsn := "sqlite", ""
	if o.DatabaseURL == "" {
		// 开库前先自检：SQLite 的 "unable to open database file (14)" 只表明写不了，
		// 不说明是谁写不了、该怎么修。在容器里这几乎总是 bind mount 属主问题。
		if err := checkWritable(o.Dir); err != nil {
			return nil, err
		}
		dsn = filepath.Join(o.Dir, "relay.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	} else {
		d = dialectPostgres
		// pgx 的 database/sql 驱动注册名是 "pgx"，不是 "postgres"。
		driver, dsn = "pgx", o.DatabaseURL
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// sql.Open 是惰性的：DSN 写错或数据库还没起来，要等第一条查询才暴露。
	// compose 里 postgres 可能比 relay 晚就绪，所以这里带重试地 Ping。
	if d == dialectPostgres {
		if err := pingWithRetry(db, 30*time.Second); err != nil {
			db.Close()
			return nil, fmt.Errorf("连接 Postgres 失败: %w", err)
		}
	}

	maxOpen, maxIdle := o.MaxOpen, o.MaxIdle
	if maxOpen <= 0 {
		if d == dialectPostgres {
			maxOpen = 20
		} else {
			// 纯 Go 驱动并发写会锁竞争，串行化会话；读多写少的场景够用。
			maxOpen = 1
		}
	}
	if maxIdle <= 0 {
		if d == dialectPostgres {
			maxIdle = 10
		} else {
			maxIdle = 1
		}
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)

	s := &Store{db: db, cipher: c, dialect: d}
	s.keys.hits = map[string]keyCacheEntry{}
	s.keys.touched = map[string]time.Time{}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	// 先停异步写手再关库，否则最后一批流水会撞上 closed database。
	s.StopUsage()
	return s.db.Close()
}

// Dialect 返回当前方言，供需要逐条改写 SQL 的调用方判断。
func (s *Store) Dialect() dialect { return s.dialect }

// likeOp 返回大小写匹配操作符。
//
// SQLite 的 LIKE 对 ASCII 不区分大小写，Postgres 的 LIKE 区分。
// 面板搜索是给人用的，两边的语义应该一致，所以 PG 下改用 ILIKE。
func (d dialect) likeOp() string {
	if d == dialectPostgres {
		return "ILIKE"
	}
	return "LIKE"
}

// sumInt 返回把 BIGINT 聚合结果转回 int64 的表达式。
//
// Postgres 的 SUM(bigint) 返回 numeric，pgx 的 database/sql 分支会按字符串
// 返回，Scan 到 int64 会失败；显式 ::bigint 才能稳定扫描。
// 不使用占位符，因此不需要 rebind。
func (d dialect) sumInt(expr string) string {
	if d == dialectPostgres {
		return "COALESCE(SUM(" + expr + "), 0)::bigint"
	}
	return "COALESCE(SUM(" + expr + "), 0)"
}

// escapeLike 转义 LIKE 的通配符。用户搜索 "50%" 时不应该匹配到任意内容。
// 两个方言都支持 ESCAPE '\'。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return "%" + s + "%"
}

// ---- 查询包装：所有业务 SQL 从这里走，保证 Postgres 下占位符被正确改写 ----

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.dialect.rebind(q), args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.dialect.rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.dialect.rebind(q), args...)
}

// ExecContext / QueryRowContext 是导出包装，供测试与少量跨包调用使用。
func (s *Store) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.exec(ctx, q, args...)
}

func (s *Store) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return s.queryRow(ctx, q, args...)
}

// Exec 是 context.Background 的便捷版本，供测试使用。
func (s *Store) Exec(q string, args ...any) (sql.Result, error) {
	return s.exec(context.Background(), q, args...)
}

// beginTx 开启事务。事务内的语句必须走 execTx/queryTx/queryRowTx。
func (s *Store) beginTx(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}

func (s *Store) execTx(ctx context.Context, tx *sql.Tx, q string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, s.dialect.rebind(q), args...)
}

func (s *Store) queryTx(ctx context.Context, tx *sql.Tx, q string, args ...any) (*sql.Rows, error) {
	return tx.QueryContext(ctx, s.dialect.rebind(q), args...)
}

func (s *Store) queryRowTx(ctx context.Context, tx *sql.Tx, q string, args ...any) *sql.Row {
	return tx.QueryRowContext(ctx, s.dialect.rebind(q), args...)
}

// checkWritable 实测数据目录能否创建文件，失败时给出可直接照做的修复命令。
// 只做探测，不留下任何文件。
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".writecheck-*")
	if err == nil {
		name := f.Name()
		f.Close()
		os.Remove(name)
		return nil
	}
	return fmt.Errorf(`数据目录 %s 不可写：%v

容器里出现这个错误，几乎都是 bind mount 的属主问题：
镜像里的 chown 在构建时就完成了，挂载宿主目录会覆盖掉它，
容器内的进程（uid 10001 relay）拿不到写权限。

修复（宿主机上执行）：
  sudo chown -R 10001:10001 <你的数据目录>

或者用最新镜像，入口脚本会在启动时自动修正属主。`, dir, err)
}

// pingWithRetry 在超时内反复 Ping，给数据库容器留出启动时间。
func pingWithRetry(db *sql.DB, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		lastErr = db.PingContext(ctx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(2 * time.Second)
	}
}

func (s *Store) migrate() error {
	schema := sqliteSchema
	if s.dialect == dialectPostgres {
		schema = pgSchema
		// Postgres 并发启动时，CREATE TABLE IF NOT EXISTS 仍可能撞
		// pg_type_typname_nsp_index。整段 DDL 用事务级 advisory lock 串行化，
		// 事务结束自动释放。
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("开启迁移事务失败: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(720854120031)`); err != nil {
			return fmt.Errorf("获取迁移锁失败: %w", err)
		}
		for _, stmt := range schema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("建表失败: %w", err)
			}
		}
		if err := s.ensureAccountColumnsTx(ctx, tx); err != nil {
			return err
		}
		// 报表聚合表同理要补列。老版本建出来的表缺「输入/输出 Token」「成功数」，
		// 不补的话新聚合 SQL 会整页 500。和 accounts 一样必须放在事务里走，
		// 多副本启动时由上面的 advisory lock 串行化。
		if err := s.ensureReportColumnsTx(ctx, tx); err != nil {
			return err
		}
		if err := s.applySchemaVersionTx(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	// CREATE TABLE IF NOT EXISTS 不会给已存在的表补列，老库升级要单独处理。
	if err := s.ensureAccountColumns(); err != nil {
		return err
	}
	// 报表聚合表同理：上一版建出来的表缺列，必须补齐才能跑新聚合 SQL。
	// 建表本身由上面的 schema 保证（都带 IF NOT EXISTS），这里只补列。
	if err := s.ensureReportColumns(); err != nil {
		return err
	}
	return s.applySchemaVersion()
}

// accountAddedColumns 是 accounts 表后来新增、需要给老库补齐的列。
// 顺序固定、只追加不改名：SQLite 的 ALTER 与 Postgres 的 ADD COLUMN 都是增量操作。
var accountAddedColumns = []struct{ name, ddl string }{
	{"max_concurrent", `ALTER TABLE accounts ADD COLUMN max_concurrent %INTEGER_OR_BIGINT% NOT NULL DEFAULT 0`},
	{"lease_until", `ALTER TABLE accounts ADD COLUMN lease_until %INTEGER_OR_BIGINT% NOT NULL DEFAULT 0`},
	{"provider", `ALTER TABLE accounts ADD COLUMN provider TEXT NOT NULL DEFAULT 'raccoon'`},
	{"sn_username", `ALTER TABLE accounts ADD COLUMN sn_username TEXT NOT NULL DEFAULT ''`},
	{"sn_password", `ALTER TABLE accounts ADD COLUMN sn_password TEXT NOT NULL DEFAULT ''`},
	{"sn_token", `ALTER TABLE accounts ADD COLUMN sn_token TEXT NOT NULL DEFAULT ''`},
	{"sn_token_exp", `ALTER TABLE accounts ADD COLUMN sn_token_exp %INTEGER_OR_BIGINT% NOT NULL DEFAULT 0`},
	{"sn_refresh_token", `ALTER TABLE accounts ADD COLUMN sn_refresh_token TEXT NOT NULL DEFAULT ''`},
	{"sn_pool", `ALTER TABLE accounts ADD COLUMN sn_pool TEXT NOT NULL DEFAULT ''`},
	{"sn_pool_at", `ALTER TABLE accounts ADD COLUMN sn_pool_at %INTEGER_OR_BIGINT% NOT NULL DEFAULT 0`},
}

// intType 按方言给时间戳类型：Postgres 必须 BIGINT，SQLite 用 INTEGER 即可。
func (s *Store) intType() string {
	if s.dialect == dialectPostgres {
		return "BIGINT"
	}
	return "INTEGER"
}

func (s *Store) ensureAccountColumns() error {
	for _, col := range accountAddedColumns {
		has, err := columnExists(s.db, "accounts", col.name)
		if err != nil {
			return fmt.Errorf("检查 accounts.%s 失败: %w", col.name, err)
		}
		if has {
			continue
		}
		ddl := strings.ReplaceAll(col.ddl, "%INTEGER_OR_BIGINT%", s.intType())
		if _, err := s.db.Exec(ddl); err != nil {
			return fmt.Errorf("补充 accounts.%s 失败: %w", col.name, err)
		}
	}
	return nil
}

// ensureAccountColumnsTx 是 Postgres 迁移事务里的同一逻辑，必须走 tx 才能在
// advisory lock 内串行化，避免多副本同时 ALTER 撞 errDuplicateColumn。
func (s *Store) ensureAccountColumnsTx(ctx context.Context, tx *sql.Tx) error {
	for _, col := range accountAddedColumns {
		has, err := columnExistsTx(ctx, tx, "accounts", col.name)
		if err != nil {
			return fmt.Errorf("检查 accounts.%s 失败: %w", col.name, err)
		}
		if has {
			continue
		}
		// 这里的 DDL 不含占位符（表名/列名是代码里的常量），不能走 rebind：
		// PG 的 rebind 会把 DDL 里的 ? 当占位符处理，虽然当前没有 ?，但语义上不该经过它。
		ddl := strings.ReplaceAll(col.ddl, "%INTEGER_OR_BIGINT%", s.intType())
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("补充 accounts.%s 失败: %w", col.name, err)
		}
	}
	return nil
}

// ensureReportColumns 给上一版报表聚合表补上「输入/输出 Token」「成功请求数」列。
//
// 聚合表是 CREATE TABLE IF NOT EXISTS 建的：上一版已经建出来的表不会自动多出
// 这些列，不补的话新聚合 SQL 会直接报「no such column」，报表页整页 500。
// 新表本身（request_daily_api）由 schema 段负责创建，这里只管老表补列。
//
// 两种方言共用同一份补列清单，避免两边漏改一边。
type reportColumnPatch struct{ table, column, ddl string }

var reportColumnPatches = []reportColumnPatch{
	{"request_daily", "prompt_tokens", `ALTER TABLE request_daily ADD COLUMN prompt_tokens BIGINT NOT NULL DEFAULT 0`},
	{"request_daily", "completion_tokens", `ALTER TABLE request_daily ADD COLUMN completion_tokens BIGINT NOT NULL DEFAULT 0`},
	{"request_daily_model", "successful", `ALTER TABLE request_daily_model ADD COLUMN successful BIGINT NOT NULL DEFAULT 0`},
	{"request_daily_model", "prompt_tokens", `ALTER TABLE request_daily_model ADD COLUMN prompt_tokens BIGINT NOT NULL DEFAULT 0`},
	{"request_daily_model", "completion_tokens", `ALTER TABLE request_daily_model ADD COLUMN completion_tokens BIGINT NOT NULL DEFAULT 0`},
}

func (s *Store) ensureReportColumns() error {
	for _, p := range reportColumnPatches {
		has, err := s.columnExistsAny(p.table, p.column)
		if err != nil {
			return fmt.Errorf("检查 %s.%s 列失败: %w", p.table, p.column, err)
		}
		if has {
			continue
		}
		if _, err := s.db.Exec(p.ddl); err != nil {
			return fmt.Errorf("补充 %s.%s 失败: %w", p.table, p.column, err)
		}
	}
	return nil
}

// ensureReportColumnsTx 是 Postgres 迁移事务里的同一逻辑。
func (s *Store) ensureReportColumnsTx(ctx context.Context, tx *sql.Tx) error {
	for _, p := range reportColumnPatches {
		has, err := columnExistsTx(ctx, tx, p.table, p.column)
		if err != nil {
			return fmt.Errorf("检查 %s.%s 列失败: %w", p.table, p.column, err)
		}
		if has {
			continue
		}
		if _, err := s.execTx(ctx, tx, p.ddl); err != nil {
			return fmt.Errorf("补充 %s.%s 失败: %w", p.table, p.column, err)
		}
	}
	return nil
}

// columnExistsAny 跨方言判断表列是否存在。
func (s *Store) columnExistsAny(table, column string) (bool, error) {
	if s.dialect != dialectPostgres {
		return columnExists(s.db, table, column)
	}
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_name = $1 AND column_name = $2`, table, column).Scan(&n)
	return n > 0, err
}

// applySchemaVersion 记录 schema 版本（SQLite 路径）。
//
// SQLite 继续使用 PRAGMA user_version，保证旧库升级路径不变；
// 同时写一行 schema_migrations，方便未来统一切换。
func (s *Store) applySchemaVersion() error {
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}
	if ver >= schemaVersion {
		return nil
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return s.recordSchemaVersion(s.db, time.Now().UnixMilli())
}

// applySchemaVersionTx 是 Postgres 路径：在同一个迁移事务里记录版本。
func (s *Store) applySchemaVersionTx(ctx context.Context, tx *sql.Tx) error {
	return s.recordSchemaVersion(tx, time.Now().UnixMilli())
}

// recordSchemaVersion 在 schema_migrations 里写入当前版本（幂等）。
func (s *Store) recordSchemaVersion(ex interface {
	Exec(string, ...any) (sql.Result, error)
}, at int64) error {
	_, err := ex.Exec(s.dialect.rebind(
		`INSERT INTO schema_migrations (version, name, applied_at)
		 VALUES (?, 'schema', ?)
		 ON CONFLICT (version) DO NOTHING`),
		schemaVersion, at)
	return err
}

// DB 只留给 SQLite 专用测试；业务代码不要用它。
func (s *Store) DB() *sql.DB { return s.db }
