package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// 迁移标记：写进 migration_state，表示 relay.db 已经导入过。
const migrateStateName = "sqlite-import"

// migrateAdvisoryLock 是迁移专用的事务级锁 key。
// 与建表 DDL 的锁分开，避免互相阻塞。
const migrateAdvisoryLock = 720854120032

// migrationState 是迁移状态机的落库记录。
type migrationState struct {
	State      string
	StartedAt  int64
	FinishedAt int64
	Detail     string
}

// sqliteDump 是从旧库读出的原始行。密文列保持密文，不在这里解密。
type sqliteDump struct {
	Accounts     []accountRow
	Keys         []keyRow
	ModelCools   []modelCoolRow
	AccountCools []accountCoolRow
	Checkins     []checkinRow
	Usage        []usageRow
	Settings     []settingRow
	Events       []eventRow
}

type accountRow struct {
	ID, UID, Name, Note, Token, RefreshToken                                                                   string
	TokenExp, Credits, CreditsAt                                                                               int64
	Status                                                                                                     string
	CoolUntil                                                                                                  int64
	CoolReason                                                                                                 string
	ExpiresAt                                                                                                  int64
	DeviceID, DeviceName, OSVersion, OfficeIdentity, Platform, ClientChannel, Language, Meta, LastCheckinDay    string
	Priority, Enabled, CreatedAt, UpdatedAt, LastUsed                                                          int64
}

type keyRow struct {
	ID, Name, Prefix, Hash string
	Enabled, CreatedAt, LastUsed int64
}

type modelCoolRow struct {
	AccountID, Model string
	Until            int64
	Reason           string
	Hits             int64
}

type accountCoolRow struct {
	AccountID string
	Until     int64
	Reason    string
	Hits      int64
}

type checkinRow struct {
	ID                                  int64
	AccountID, Day, Action, Source      string
	OK, Already, Credit                 int64
	Message                             string
	At                                  int64
}

type usageRow struct {
	ID               int64
	TS               int64
	AccountID        string
	Model, API       string
	Status           int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	MS               int64
	Error            string
}

type settingRow struct {
	Key, Value string
}

type eventRow struct {
	ID        int64
	TS        int64
	Level     string
	Scope     string
	Component string
	AccountID string
	Message   string
	Detail    string
	Count     int64
	LastTS    int64
}

// sqliteDumpFiles 列出旧库可能存在的三个文件（主库 + WAL + SHM）。
func sqliteDumpFiles(dir string) []string {
	base := filepath.Join(dir, "relay.db")
	return []string{base, base + "-wal", base + "-shm"}
}

// checkpointAndRename 把旧库落盘并改名为 importing 状态，返回新的主库路径。
//
// WAL 里可能有尚未合并进主库的最新行，直接只读主库会静默丢数据。
// 先可写打开执行 checkpoint，再改名：Windows 下旧进程还占着文件时改名会失败，
// 这正好当作「旧实例仍在运行」的防护。
func checkpointAndRename(dir string) (string, error) {
	src := filepath.Join(dir, "relay.db")
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("旧库不存在: %w", err)
	}

	db, err := sql.Open("sqlite", src)
	if err != nil {
		return "", fmt.Errorf("打开旧库失败: %w", err)
	}
	_, cerr := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	closeErr := db.Close()
	if cerr != nil {
		return "", fmt.Errorf("WAL checkpoint 失败: %w", cerr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("关闭旧库失败: %w", closeErr)
	}

	stamp := time.Now().Unix()
	dst := filepath.Join(dir, fmt.Sprintf("relay.db.importing-%d", stamp))
	// 先改主库，再改附属文件：任何一步失败都把它们改回原名。
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("改名旧库失败（可能仍有进程占用）: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		old := src + suffix
		if _, err := os.Stat(old); err == nil {
			if err := os.Rename(old, dst+suffix); err != nil {
				// 改不回去就把主库还原，保持现场可重试。
				_ = os.Rename(dst, src)
				return "", fmt.Errorf("改名 %s 失败: %w", suffix, err)
			}
		}
	}
	return dst, nil
}

// restoreDumpFiles 在迁移失败时把 importing 文件改回原名。
func restoreDumpFiles(importing string) {
	src := filepath.Join(filepath.Dir(importing), "relay.db")
	if err := os.Rename(importing, src); err != nil {
		return
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(importing + suffix); err == nil {
			_ = os.Rename(importing+suffix, src+suffix)
		}
	}
}

// finishDumpFiles 把 importing 改名为 migrated，表示迁移成功。
func finishDumpFiles(importing string) error {
	dst := importing + ".migrated"
	return os.Rename(importing, dst)
}

// readSQLiteDump 以只读方式读出旧库全部行。可脱离 Postgres 单测。
//
// 旧版本可能没有后来新增的表（例如 v1.2.x 才有 account_cooldowns），
// 缺失的表按空集处理，而不是让整个迁移失败。
func readSQLiteDump(path string) (*sqliteDump, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	d := &sqliteDump{}
	// accounts / api_keys 是基础表，缺失说明这不是一个 relay 库，直接报错。
	if d.Accounts, err = readAccounts(db); err != nil {
		return nil, fmt.Errorf("读取 accounts: %w", err)
	}
	if d.Keys, err = readKeys(db); err != nil {
		return nil, fmt.Errorf("读取 api_keys: %w", err)
	}
	if d.ModelCools, err = readIfExists(db, "读取 model_cooldowns", readModelCools); err != nil {
		return nil, err
	}
	if d.AccountCools, err = readIfExists(db, "读取 account_cooldowns", readAccountCools); err != nil {
		return nil, err
	}
	if d.Checkins, err = readIfExists(db, "读取 checkin_history", readCheckins); err != nil {
		return nil, err
	}
	if d.Usage, err = readIfExists(db, "读取 usage_logs", readUsage); err != nil {
		return nil, err
	}
	if d.Settings, err = readIfExists(db, "读取 settings", readSettings); err != nil {
		return nil, err
	}
	if d.Events, err = readIfExists(db, "读取 runtime_events", readEvents); err != nil {
		return nil, err
	}
	return d, nil
}

// readIfExists 在表不存在时返回空集；其它错误照常上抛。
func readIfExists[T any](db *sql.DB, what string, fn func(*sql.DB) ([]T, error)) ([]T, error) {
	rows, err := fn(db)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return rows, nil
}

func readAccounts(db *sql.DB) ([]accountRow, error) {
	rows, err := db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []accountRow
	for rows.Next() {
		var a accountRow
		if err := rows.Scan(&a.ID, &a.UID, &a.Name, &a.Note, &a.Token, &a.RefreshToken, &a.TokenExp,
			&a.Credits, &a.CreditsAt, &a.Status, &a.CoolUntil, &a.CoolReason, &a.ExpiresAt,
			&a.DeviceID, &a.DeviceName, &a.OSVersion, &a.OfficeIdentity, &a.Platform,
			&a.ClientChannel, &a.Language, &a.Meta, &a.Priority, &a.Enabled,
			&a.LastCheckinDay, &a.CreatedAt, &a.UpdatedAt, &a.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func readKeys(db *sql.DB) ([]keyRow, error) {
	rows, err := db.Query(`SELECT id, name, prefix, hash, enabled, created_at, last_used FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []keyRow
	for rows.Next() {
		var k keyRow
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.Enabled, &k.CreatedAt, &k.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func readModelCools(db *sql.DB) ([]modelCoolRow, error) {
	rows, err := db.Query(`SELECT account_id, model, "until", reason, hits FROM model_cooldowns ORDER BY account_id, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []modelCoolRow
	for rows.Next() {
		var c modelCoolRow
		if err := rows.Scan(&c.AccountID, &c.Model, &c.Until, &c.Reason, &c.Hits); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func readAccountCools(db *sql.DB) ([]accountCoolRow, error) {
	rows, err := db.Query(`SELECT account_id, "until", reason, hits FROM account_cooldowns ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []accountCoolRow
	for rows.Next() {
		var c accountCoolRow
		if err := rows.Scan(&c.AccountID, &c.Until, &c.Reason, &c.Hits); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func readCheckins(db *sql.DB) ([]checkinRow, error) {
	// source 是后来加的列，旧库没有时按空串处理。
	hasSource, err := columnExists(db, "checkin_history", "source")
	if err != nil {
		return nil, err
	}
	cols := `id, account_id, day, action, ok, already, credit, message, at`
	if hasSource {
		cols = `id, account_id, day, action, source, ok, already, credit, message, at`
	}
	rows, err := db.Query(`SELECT ` + cols + ` FROM checkin_history ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []checkinRow
	for rows.Next() {
		var r checkinRow
		if hasSource {
			if err := rows.Scan(&r.ID, &r.AccountID, &r.Day, &r.Action, &r.Source,
				&r.OK, &r.Already, &r.Credit, &r.Message, &r.At); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&r.ID, &r.AccountID, &r.Day, &r.Action,
				&r.OK, &r.Already, &r.Credit, &r.Message, &r.At); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// columnExists 用 PRAGMA table_info 判断列是否存在。
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func readUsage(db *sql.DB) ([]usageRow, error) {
	rows, err := db.Query(`SELECT id, ts, account_id, model, api, status, prompt_tokens, completion_tokens, total_tokens, ms, error FROM usage_logs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usageRow
	for rows.Next() {
		var r usageRow
		if err := rows.Scan(&r.ID, &r.TS, &r.AccountID, &r.Model, &r.API, &r.Status,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.MS, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func readSettings(db *sql.DB) ([]settingRow, error) {
	rows, err := db.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []settingRow
	for rows.Next() {
		var r settingRow
		if err := rows.Scan(&r.Key, &r.Value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func readEvents(db *sql.DB) ([]eventRow, error) {
	rows, err := db.Query(`SELECT id, ts, level, scope, component, account_id, message, detail, "count", last_ts FROM runtime_events ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var r eventRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Level, &r.Scope, &r.Component, &r.AccountID,
			&r.Message, &r.Detail, &r.Count, &r.LastTS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// verifyDumpCipher 逐条校验非空 token 能否用当前 SECRET_KEY 解开。
//
// 迁移只复制密文、不解密，所以 SECRET_KEY 不一致时导入会「成功」，
// 但之后每个号 scanAccount 都会失败、号池直接变空。必须在这里拦住。
func verifyDumpCipher(c *Cipher, d *sqliteDump) error {
	for _, a := range d.Accounts {
		if a.Token != "" {
			if _, err := c.Decrypt(a.Token); err != nil {
				return fmt.Errorf("账号 %s 的 token 无法用当前 SECRET_KEY 解密：请确认与旧部署使用相同的 SECRET_KEY", a.ID)
			}
		}
		if a.RefreshToken != "" {
			if _, err := c.Decrypt(a.RefreshToken); err != nil {
				return fmt.Errorf("账号 %s 的 refresh_token 无法解密：请确认 SECRET_KEY 一致", a.ID)
			}
		}
	}
	return nil
}

// importDump 把整份数据写入当前库。调用方负责事务、行数校验与标记。
// 使用 multi-row VALUES 批量插入；usage_logs 可能几十万行，逐行 Exec 太慢。
//
// 最后的 setval 只对 Postgres 有意义（SQLite 的 AUTOINCREMENT 会跟着显式 id 走）。
func (s *Store) importDump(ctx context.Context, tx *sql.Tx, d *sqliteDump) error {
	if err := s.insertDumpRows(ctx, tx, d); err != nil {
		return err
	}
	// 显式插入 id 不会推进 IDENTITY 序列，必须手动 setval，
	// 否则新数据会从 1 开始撞主键。
	for _, t := range []string{"checkin_history", "usage_logs", "runtime_events"} {
		q := fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%s','id'),
				COALESCE((SELECT MAX(id) FROM %s), 0) + 1, false)`, t, t)
		if _, err := s.execTx(ctx, tx, q); err != nil {
			return fmt.Errorf("重置 %s 序列: %w", t, err)
		}
	}
	return nil
}

// importDumpForTest 与 importDump 相同，但跳过 Postgres 的序列重置，
// 让导入逻辑能在 SQLite 上被单测覆盖。
func (s *Store) importDumpForTest(ctx context.Context, tx *sql.Tx, d *sqliteDump) error {
	return s.insertDumpRows(ctx, tx, d)
}

func (s *Store) insertDumpRows(ctx context.Context, tx *sql.Tx, d *sqliteDump) error {
	const batch = 500

	for i := 0; i < len(d.Accounts); i += batch {
		end := min(i+batch, len(d.Accounts))
		var (
			args []any
			ph   []string
		)
		for _, a := range d.Accounts[i:end] {
			ph = append(ph, "(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, a.ID, a.UID, a.Name, a.Note, a.Token, a.RefreshToken, a.TokenExp,
				a.Credits, a.CreditsAt, a.Status, a.CoolUntil, a.CoolReason, a.ExpiresAt,
				a.DeviceID, a.DeviceName, a.OSVersion, a.OfficeIdentity, a.Platform,
				a.ClientChannel, a.Language, a.Meta, a.Priority, a.Enabled,
				a.LastCheckinDay, a.CreatedAt, a.UpdatedAt, a.LastUsed)
		}
		q := `INSERT INTO accounts (` + accountCols + `) VALUES ` + joinStrings(ph, ",")
		if _, err := s.execTx(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("导入 accounts: %w", err)
		}
	}

	for i := 0; i < len(d.Keys); i += batch {
		end := min(i+batch, len(d.Keys))
		var (
			args []any
			ph   []string
		)
		for _, k := range d.Keys[i:end] {
			ph = append(ph, "(?,?,?,?,?,?,?)")
			args = append(args, k.ID, k.Name, k.Prefix, k.Hash, k.Enabled, k.CreatedAt, k.LastUsed)
		}
		q := `INSERT INTO api_keys (id, name, prefix, hash, enabled, created_at, last_used) VALUES ` + joinStrings(ph, ",")
		if _, err := s.execTx(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("导入 api_keys: %w", err)
		}
	}

	for i := 0; i < len(d.ModelCools); i += batch {
		end := min(i+batch, len(d.ModelCools))
		var (
			args []any
			ph   []string
		)
		for _, c := range d.ModelCools[i:end] {
			ph = append(ph, "(?,?,?,?,?)")
			args = append(args, c.AccountID, c.Model, c.Until, c.Reason, c.Hits)
		}
		q := `INSERT INTO model_cooldowns (account_id, model, "until", reason, hits) VALUES ` + joinStrings(ph, ",")
		if _, err := s.execTx(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("导入 model_cooldowns: %w", err)
		}
	}

	for i := 0; i < len(d.AccountCools); i += batch {
		end := min(i+batch, len(d.AccountCools))
		var (
			args []any
			ph   []string
		)
		for _, c := range d.AccountCools[i:end] {
			ph = append(ph, "(?,?,?,?)")
			args = append(args, c.AccountID, c.Until, c.Reason, c.Hits)
		}
		q := `INSERT INTO account_cooldowns (account_id, "until", reason, hits) VALUES ` + joinStrings(ph, ",")
		if _, err := s.execTx(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("导入 account_cooldowns: %w", err)
		}
	}

	for i := 0; i < len(d.Checkins); i += batch {
		end := min(i+batch, len(d.Checkins))
		var (
			args []any
			ph   []string
		)
		for _, r := range d.Checkins[i:end] {
			ph = append(ph, "(?,?,?,?,?,?,?,?,?,?)")
			args = append(args, r.ID, r.AccountID, r.Day, r.Action, r.Source,
				r.OK, r.Already, r.Credit, r.Message, r.At)
		}
		q := `INSERT INTO checkin_history (id, account_id, day, action, source, ok, already, credit, message, at) VALUES ` + joinStrings(ph, ",")
		if _, err := s.execTx(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("导入 checkin_history: %w", err)
		}
	}

	for i := 0; i < len(d.Usage); i += batch {
		end := min(i+batch, len(d.Usage))
		var (
			args []any
			ph   []string
		)
		for _, r := range d.Usage[i:end] {
			ph = append(ph, "(?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, r.ID, r.TS, r.AccountID, r.Model, r.API, r.Status,
				r.PromptTokens, r.CompletionTokens, r.TotalTokens, r.MS, r.Error)
		}
		q := `INSERT INTO usage_logs (id, ts, account_id, model, api, status, prompt_tokens, completion_tokens, total_tokens, ms, error) VALUES ` + joinStrings(ph, ",")
		if _, err := s.execTx(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("导入 usage_logs: %w", err)
		}
	}

	for i := 0; i < len(d.Settings); i += batch {
		end := min(i+batch, len(d.Settings))
		var (
			args []any
			ph   []string
		)
		for _, r := range d.Settings[i:end] {
			ph = append(ph, "(?,?)")
			args = append(args, r.Key, r.Value)
		}
		q := `INSERT INTO settings (key, value) VALUES ` + joinStrings(ph, ",")
		if _, err := s.execTx(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("导入 settings: %w", err)
		}
	}

	for i := 0; i < len(d.Events); i += batch {
		end := min(i+batch, len(d.Events))
		var (
			args []any
			ph   []string
		)
		for _, r := range d.Events[i:end] {
			ph = append(ph, "(?,?,?,?,?,?,?,?,?,?)")
			args = append(args, r.ID, r.TS, r.Level, r.Scope, r.Component, r.AccountID,
				r.Message, r.Detail, r.Count, r.LastTS)
		}
		q := `INSERT INTO runtime_events (id, ts, level, scope, component, account_id, message, detail, "count", last_ts) VALUES ` + joinStrings(ph, ",")
		if _, err := s.execTx(ctx, tx, q, args...); err != nil {
			return fmt.Errorf("导入 runtime_events: %w", err)
		}
	}
	return nil
}

func joinStrings(items []string, sep string) string {
	out := ""
	for i, it := range items {
		if i > 0 {
			out += sep
		}
		out += it
	}
	return out
}

// dumpCounts 返回每张表的行数，用于导入前后比对。
func dumpCounts(d *sqliteDump) map[string]int {
	return map[string]int{
		"accounts":          len(d.Accounts),
		"api_keys":          len(d.Keys),
		"model_cooldowns":   len(d.ModelCools),
		"account_cooldowns": len(d.AccountCools),
		"checkin_history":   len(d.Checkins),
		"usage_logs":        len(d.Usage),
		"settings":          len(d.Settings),
		"runtime_events":    len(d.Events),
	}
}

// verifyTargetCounts 逐表比对目标库行数，任一不等即返回错误。
func (s *Store) verifyTargetCounts(ctx context.Context, tx *sql.Tx, d *sqliteDump) error {
	want := dumpCounts(d)
	for table, n := range want {
		var got int64
		if err := s.queryRowTx(ctx, tx, `SELECT COUNT(*) FROM `+table).Scan(&got); err != nil {
			return fmt.Errorf("统计 %s: %w", table, err)
		}
		if int(got) != n {
			return fmt.Errorf("表 %s 行数不一致：源 %d，目标 %d", table, n, got)
		}
	}
	return nil
}

// targetIsEmpty 判断目标库是否除了元数据表外都没有数据。
// 只检查用户数据表：schema_migrations / migration_state 是迁移自身的记账。
func (s *Store) targetIsEmpty(ctx context.Context) (bool, error) {
	for _, table := range []string{"accounts", "api_keys", "model_cooldowns", "account_cooldowns",
		"checkin_history", "usage_logs", "settings", "runtime_events"} {
		var n int64
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil
		}
	}
	return true, nil
}