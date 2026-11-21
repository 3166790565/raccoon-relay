package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MigrateMode 控制启动时旧 SQLite 库的迁移行为。
type MigrateMode string

const (
	// MigrateAuto：有旧库且目标全空才迁移，其余情况跳过。
	MigrateAuto MigrateMode = "auto"
	// MigrateOff：完全不迁移。
	MigrateOff MigrateMode = "off"
	// MigrateRequire：没有旧库也要求迁移（库被误删时尽早失败）。
	MigrateRequire MigrateMode = "require"
)

// ParseMigrateMode 解析 MIGRATE_SQLITE，非法值回落 auto。
func ParseMigrateMode(s string) MigrateMode {
	switch MigrateMode(s) {
	case MigrateOff:
		return MigrateOff
	case MigrateRequire:
		return MigrateRequire
	}
	return MigrateAuto
}

// MigrateResult 描述一次迁移尝试的结果。
type MigrateResult struct {
	Ran     bool   // 是否真的执行了导入
	Skipped bool   // 是否因条件不满足跳过
	Reason  string // 跳过或失败原因；成功时为空
	Backup  string // 成功后旧库被改名到的路径
	Counts  map[string]int
}

// MigrateSQLite 把 $Dir/relay.db 导入当前 Postgres 库。
//
// 只在 Store 是 Postgres 方言时有效；SQLite 调用直接跳过。
// 整个导入在一个事务里完成，首句取事务级 advisory lock 串行化并发实例；
// 任何一步失败都回滚，并把旧库文件改回原名，方便修复后重试。
func (s *Store) MigrateSQLite(ctx context.Context, dir string, mode MigrateMode) (MigrateResult, error) {
	if s.dialect != dialectPostgres {
		return MigrateResult{Skipped: true, Reason: "当前不是 Postgres，无需迁移"}, nil
	}
	if mode == MigrateOff {
		return MigrateResult{Skipped: true, Reason: "MIGRATE_SQLITE=off"}, nil
	}

	if err := s.ensureMigrationStateTable(ctx); err != nil {
		return MigrateResult{}, err
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return MigrateResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// 事务级 advisory lock：第二个实例会在这里等待，拿到锁后重新判断状态。
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SELECT pg_advisory_xact_lock(%d)`, migrateAdvisoryLock)); err != nil {
		return MigrateResult{}, fmt.Errorf("获取迁移锁失败: %w", err)
	}

	state, err := s.readMigrationState(ctx, tx)
	if err != nil {
		return MigrateResult{}, err
	}
	if state != nil && state.State == "done" {
		return MigrateResult{Skipped: true, Reason: "已迁移过"}, nil
	}

	empty, err := s.targetIsEmptyTx(ctx, tx)
	if err != nil {
		return MigrateResult{}, err
	}
	if !empty {
		return MigrateResult{Skipped: true, Reason: "目标库已有数据，不做自动导入"}, nil
	}

	importing, err := checkpointAndRename(dir)
	if err != nil {
		if mode == MigrateRequire {
			return MigrateResult{}, err
		}
		return MigrateResult{Skipped: true, Reason: err.Error()}, nil
	}

	dump, err := readSQLiteDump(importing)
	if err != nil {
		restoreDumpFiles(importing)
		return MigrateResult{}, err
	}
	if err := verifyDumpCipher(s.cipher, dump); err != nil {
		restoreDumpFiles(importing)
		return MigrateResult{}, err
	}
	if err := s.importDump(ctx, tx, dump); err != nil {
		restoreDumpFiles(importing)
		return MigrateResult{}, err
	}
	if err := s.verifyTargetCounts(ctx, tx, dump); err != nil {
		restoreDumpFiles(importing)
		return MigrateResult{}, err
	}
	if err := s.writeMigrationState(ctx, tx, "done", ""); err != nil {
		restoreDumpFiles(importing)
		return MigrateResult{}, err
	}
	if err := tx.Commit(); err != nil {
		restoreDumpFiles(importing)
		return MigrateResult{}, err
	}

	// 事务已提交，此时崩溃也不会重复导入；改名只是收尾。
	if err := finishDumpFiles(importing); err != nil {
		return MigrateResult{
			Ran: true, Reason: "导入成功，但旧库改名失败: " + err.Error(), Counts: dumpCounts(dump),
		}, nil
	}
	return MigrateResult{Ran: true, Backup: importing + ".migrated", Counts: dumpCounts(dump)}, nil
}

// ensureMigrationStateTable 保证状态表存在。
// schema 里没有建它：SQLite 用户不需要，Postgres 首次迁移时才建。
func (s *Store) ensureMigrationStateTable(ctx context.Context) error {
	_, err := s.exec(ctx, `CREATE TABLE IF NOT EXISTS migration_state (
		name        TEXT PRIMARY KEY,
		state       TEXT NOT NULL,
		started_at  BIGINT NOT NULL DEFAULT 0,
		finished_at BIGINT NOT NULL DEFAULT 0,
		detail      TEXT NOT NULL DEFAULT ''
	)`)
	return err
}

func (s *Store) readMigrationState(ctx context.Context, tx *sql.Tx) (*migrationState, error) {
	var st migrationState
	err := s.queryRowTx(ctx, tx,
		`SELECT state, started_at, finished_at, detail FROM migration_state WHERE name = ?`,
		migrateStateName).Scan(&st.State, &st.StartedAt, &st.FinishedAt, &st.Detail)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Store) writeMigrationState(ctx context.Context, tx *sql.Tx, state, detail string) error {
	now := time.Now().UnixMilli()
	_, err := s.execTx(ctx, tx,
		`INSERT INTO migration_state (name, state, started_at, finished_at, detail)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (name) DO UPDATE SET
			state = excluded.state,
			finished_at = excluded.finished_at,
			detail = excluded.detail`,
		migrateStateName, state, now, now, detail)
	return err
}

func (s *Store) targetIsEmptyTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	for _, table := range []string{"accounts", "api_keys", "model_cooldowns", "account_cooldowns",
		"checkin_history", "usage_logs", "settings", "runtime_events",
		"session_affinity", "account_backoffs"} {
		var n int64
		if err := s.queryRowTx(ctx, tx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil
		}
	}
	return true, nil
}
