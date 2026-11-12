package store

// schema 顺序执行，用 user_version 做迁移锚点。新增字段一律追加，不改旧列名。
var schema = []string{
	`CREATE TABLE IF NOT EXISTS accounts (
		id              TEXT PRIMARY KEY,
		uid             TEXT NOT NULL,
		name            TEXT NOT NULL DEFAULT '',
		note            TEXT NOT NULL DEFAULT '',
		token           TEXT NOT NULL DEFAULT '',
		refresh_token   TEXT NOT NULL DEFAULT '',
		token_exp       INTEGER NOT NULL DEFAULT 0,
		credits         INTEGER NOT NULL DEFAULT -1,
		credits_at      INTEGER NOT NULL DEFAULT 0,
		status          TEXT NOT NULL DEFAULT 'online',
		cool_until      INTEGER NOT NULL DEFAULT 0,
		cool_reason     TEXT NOT NULL DEFAULT '',
		expires_at      INTEGER NOT NULL DEFAULT 0,
		device_id       TEXT NOT NULL DEFAULT '',
		device_name     TEXT NOT NULL DEFAULT '',
		os_version      TEXT NOT NULL DEFAULT '',
		office_identity TEXT NOT NULL DEFAULT '',
		platform        TEXT NOT NULL DEFAULT '',
		client_channel  TEXT NOT NULL DEFAULT '',
		language        TEXT NOT NULL DEFAULT 'zh',
		meta            TEXT NOT NULL DEFAULT '{}',
		priority        INTEGER NOT NULL DEFAULT 0,
		enabled         INTEGER NOT NULL DEFAULT 1,
		last_checkin_day TEXT NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL DEFAULT 0,
		updated_at      INTEGER NOT NULL DEFAULT 0,
		last_used       INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_uid ON accounts(uid)`,

	`CREATE TABLE IF NOT EXISTS api_keys (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL DEFAULT '',
		prefix     TEXT NOT NULL DEFAULT '',
		hash       TEXT NOT NULL,
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL DEFAULT 0,
		last_used  INTEGER NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS model_cooldowns (
		account_id TEXT NOT NULL,
		model      TEXT NOT NULL,
		until      INTEGER NOT NULL DEFAULT 0,
		reason     TEXT NOT NULL DEFAULT '',
		hits       INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, model)
	)`,

	`CREATE TABLE IF NOT EXISTS checkin_history (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id TEXT NOT NULL,
		day        TEXT NOT NULL,
		action     TEXT NOT NULL DEFAULT '',
		ok         INTEGER NOT NULL DEFAULT 0,
		already    INTEGER NOT NULL DEFAULT 0,
		credit     INTEGER NOT NULL DEFAULT 0,
		message    TEXT NOT NULL DEFAULT '',
		at         INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_checkin_day ON checkin_history(account_id, day)`,

	`CREATE TABLE IF NOT EXISTS usage_logs (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		ts                INTEGER NOT NULL,
		account_id        TEXT NOT NULL DEFAULT '',
		model             TEXT NOT NULL DEFAULT '',
		api               TEXT NOT NULL DEFAULT '',
		status            INTEGER NOT NULL DEFAULT 0,
		prompt_tokens     INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens      INTEGER NOT NULL DEFAULT 0,
		ms                INTEGER NOT NULL DEFAULT 0,
		error             TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_logs(ts)`,

	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	)`,
}

const schemaVersion = 1
