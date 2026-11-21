package store

// schema 顺序执行。新增字段一律追加，不改旧列名。
// SQLite 与 Postgres 各一套：差异集中在类型（BIGINT vs INTEGER）、自增主键
// 和默认值写法，语句结构保持一致，便于对照维护。
var sqliteSchema = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL DEFAULT '',
		applied_at INTEGER NOT NULL DEFAULT 0
	)`,

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
		last_used       INTEGER NOT NULL DEFAULT 0,
		max_concurrent  INTEGER NOT NULL DEFAULT 0,
		-- provider 区分上游：raccoon（小浣熊）与 sensenova（商汤日日新 Token Plan）。
		-- 两种号在同一张表里，但凭据形态、额度接口、选号模型都不同，靠这一列分流。
		provider        TEXT NOT NULL DEFAULT 'raccoon',
		-- 商汤日日新专用：控制台账号密码用来换登录票、建 API Key、读积分池。
		-- 密码与令牌走同一套 AES 加密；API Key 复用 token 列。
		sn_username      TEXT NOT NULL DEFAULT '',
		sn_password      TEXT NOT NULL DEFAULT '',
		sn_token         TEXT NOT NULL DEFAULT '',
		sn_token_exp     INTEGER NOT NULL DEFAULT 0,
		sn_refresh_token TEXT NOT NULL DEFAULT '',
		sn_pool          TEXT NOT NULL DEFAULT '',
		sn_pool_at       INTEGER NOT NULL DEFAULT 0
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
	// 每个请求都按 hash 校验 Key；没有索引时是全表扫描。
	// 非唯一：历史库若存在重复 hash，UNIQUE 会让所有实例启动失败。
	`CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(hash)`,

	`CREATE TABLE IF NOT EXISTS model_cooldowns (
		account_id TEXT NOT NULL,
		model      TEXT NOT NULL,
		"until"    INTEGER NOT NULL DEFAULT 0,
		reason     TEXT NOT NULL DEFAULT '',
		hits       INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, model)
	)`,

	// 账号级失败链：200108「账号异常调用」这类上游处罚针对整个账号，换模型也没用。
	// 和模型级冷却一样按 3 → 5 → 10… 阶梯累加，所以命中次数要单独落库，
	// 否则进程重启后阶梯会退回第一档。
	`CREATE TABLE IF NOT EXISTS account_cooldowns (
		account_id TEXT PRIMARY KEY,
		"until"    INTEGER NOT NULL DEFAULT 0,
		reason     TEXT NOT NULL DEFAULT '',
		hits       INTEGER NOT NULL DEFAULT 0
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
		-- 触发来源：auto 定时 / manual 手动 / onboard 新号落地。老数据为空。
		source     TEXT NOT NULL DEFAULT '',
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
	`CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_logs(account_id, id)`,

	// 报表页的日聚合。报表要显示一年热力图与长区间趋势，直接对 usage_logs
	// 做 GROUP BY 会在大表上扫几十万行；这里在写流水的同时按天累加，
	// 报表只读这张小表。流水被清理（保留天数）后，历史日统计仍然保留。
	`CREATE TABLE IF NOT EXISTS request_daily (
		day               TEXT PRIMARY KEY,
		requests          INTEGER NOT NULL DEFAULT 0,
		successful        INTEGER NOT NULL DEFAULT 0,
		tokens            INTEGER NOT NULL DEFAULT 0,
		prompt_tokens     INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0
	)`,
	// 按「天 × 模型」的分解，报表的模型 Top N 与趋势图选维度都读它。
	`CREATE TABLE IF NOT EXISTS request_daily_model (
		day               TEXT NOT NULL,
		model             TEXT NOT NULL DEFAULT '',
		requests          INTEGER NOT NULL DEFAULT 0,
		successful        INTEGER NOT NULL DEFAULT 0,
		tokens            INTEGER NOT NULL DEFAULT 0,
		prompt_tokens     INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (day, model)
	)`,
	// 按「天 × 出站协议」的分解（openai / anthropic / responses），
	// 报表的「协议统计」与趋势图按协议选维度都读它。
	`CREATE TABLE IF NOT EXISTS request_daily_api (
		day               TEXT NOT NULL,
		api               TEXT NOT NULL DEFAULT '',
		requests          INTEGER NOT NULL DEFAULT 0,
		successful        INTEGER NOT NULL DEFAULT 0,
		tokens            INTEGER NOT NULL DEFAULT 0,
		prompt_tokens     INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (day, api)
	)`,

	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	)`,

	// 运行日志事件流：进程里发生的事（上游失败、换号、限流、签到、续期……）
	// 结构化落库，面板按级别/范围/来源/账号检索。进程日志文件只负责留原始行，
	// 排查问题靠这张表——它能按维度筛，也能翻页。
	`CREATE TABLE IF NOT EXISTS runtime_events (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		ts         INTEGER NOT NULL,
		level      TEXT NOT NULL DEFAULT 'info',
		scope      TEXT NOT NULL DEFAULT '',
		component  TEXT NOT NULL DEFAULT '',
		account_id TEXT NOT NULL DEFAULT '',
		message    TEXT NOT NULL DEFAULT '',
		detail     TEXT NOT NULL DEFAULT '',
		-- 相同事件在短时间内重复出现时合并成一条并累加次数，
		-- 否则一条 429 风暴能把列表刷满，真正的根因反而被埋掉。
		"count"    INTEGER NOT NULL DEFAULT 1,
		last_ts    INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_event_ts ON runtime_events(ts)`,
	`CREATE INDEX IF NOT EXISTS idx_event_scope ON runtime_events(scope, id)`,
	`CREATE INDEX IF NOT EXISTS idx_event_account ON runtime_events(account_id, id)`,
}

// pgSchema 与 sqliteSchema 结构一一对应，只改方言差异：
//   - 毫秒时间戳必须 BIGINT：PG 的 INTEGER 是 int4，1.7e12 会溢出；
//   - 自增主键用 GENERATED BY DEFAULT AS IDENTITY，允许迁移时显式插 id；
//   - 0/1 布尔列保持 BIGINT，不改 BOOLEAN，避免 Scan 语义变化。
var pgSchema = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    BIGINT PRIMARY KEY,
		name       TEXT NOT NULL DEFAULT '',
		applied_at BIGINT NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS accounts (
		id              TEXT PRIMARY KEY,
		uid             TEXT NOT NULL,
		name            TEXT NOT NULL DEFAULT '',
		note            TEXT NOT NULL DEFAULT '',
		token           TEXT NOT NULL DEFAULT '',
		refresh_token   TEXT NOT NULL DEFAULT '',
		token_exp       BIGINT NOT NULL DEFAULT 0,
		credits         BIGINT NOT NULL DEFAULT -1,
		credits_at      BIGINT NOT NULL DEFAULT 0,
		status          TEXT NOT NULL DEFAULT 'online',
		cool_until      BIGINT NOT NULL DEFAULT 0,
		cool_reason     TEXT NOT NULL DEFAULT '',
		expires_at      BIGINT NOT NULL DEFAULT 0,
		device_id       TEXT NOT NULL DEFAULT '',
		device_name     TEXT NOT NULL DEFAULT '',
		os_version      TEXT NOT NULL DEFAULT '',
		office_identity TEXT NOT NULL DEFAULT '',
		platform        TEXT NOT NULL DEFAULT '',
		client_channel  TEXT NOT NULL DEFAULT '',
		language        TEXT NOT NULL DEFAULT 'zh',
		meta            TEXT NOT NULL DEFAULT '{}',
		priority        BIGINT NOT NULL DEFAULT 0,
		enabled         BIGINT NOT NULL DEFAULT 1,
		last_checkin_day TEXT NOT NULL DEFAULT '',
		created_at      BIGINT NOT NULL DEFAULT 0,
		updated_at      BIGINT NOT NULL DEFAULT 0,
		last_used       BIGINT NOT NULL DEFAULT 0,
		max_concurrent  BIGINT NOT NULL DEFAULT 0,
		provider        TEXT NOT NULL DEFAULT 'raccoon',
		sn_username      TEXT NOT NULL DEFAULT '',
		sn_password      TEXT NOT NULL DEFAULT '',
		sn_token         TEXT NOT NULL DEFAULT '',
		sn_token_exp     BIGINT NOT NULL DEFAULT 0,
		sn_refresh_token TEXT NOT NULL DEFAULT '',
		sn_pool          TEXT NOT NULL DEFAULT '',
		sn_pool_at       BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_uid ON accounts(uid)`,

	`CREATE TABLE IF NOT EXISTS api_keys (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL DEFAULT '',
		prefix     TEXT NOT NULL DEFAULT '',
		hash       TEXT NOT NULL,
		enabled    BIGINT NOT NULL DEFAULT 1,
		created_at BIGINT NOT NULL DEFAULT 0,
		last_used  BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(hash)`,

	`CREATE TABLE IF NOT EXISTS model_cooldowns (
		account_id TEXT NOT NULL,
		model      TEXT NOT NULL,
		"until"    BIGINT NOT NULL DEFAULT 0,
		reason     TEXT NOT NULL DEFAULT '',
		hits       BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, model)
	)`,

	`CREATE TABLE IF NOT EXISTS account_cooldowns (
		account_id TEXT PRIMARY KEY,
		"until"    BIGINT NOT NULL DEFAULT 0,
		reason     TEXT NOT NULL DEFAULT '',
		hits       BIGINT NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS checkin_history (
		id         BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		account_id TEXT NOT NULL,
		day        TEXT NOT NULL,
		action     TEXT NOT NULL DEFAULT '',
		ok         BIGINT NOT NULL DEFAULT 0,
		already    BIGINT NOT NULL DEFAULT 0,
		credit     BIGINT NOT NULL DEFAULT 0,
		message    TEXT NOT NULL DEFAULT '',
		source     TEXT NOT NULL DEFAULT '',
		at         BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_checkin_day ON checkin_history(account_id, day)`,
	// CheckinCenterAccounts 的相关子查询按 (account_id, day) 取最新一条，
	// PG 下补上 id DESC 才能直接走索引。
	`CREATE INDEX IF NOT EXISTS idx_checkin_day_id ON checkin_history(account_id, day, id DESC)`,

	`CREATE TABLE IF NOT EXISTS usage_logs (
		id                BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		ts                BIGINT NOT NULL,
		account_id        TEXT NOT NULL DEFAULT '',
		model             TEXT NOT NULL DEFAULT '',
		api               TEXT NOT NULL DEFAULT '',
		status            BIGINT NOT NULL DEFAULT 0,
		prompt_tokens     BIGINT NOT NULL DEFAULT 0,
		completion_tokens BIGINT NOT NULL DEFAULT 0,
		total_tokens      BIGINT NOT NULL DEFAULT 0,
		ms                BIGINT NOT NULL DEFAULT 0,
		error             TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_logs(ts)`,
	`CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_logs(account_id, id)`,

	// 与 SQLite 版一一对应的报表日聚合表。
	`CREATE TABLE IF NOT EXISTS request_daily (
		day               TEXT PRIMARY KEY,
		requests          BIGINT NOT NULL DEFAULT 0,
		successful        BIGINT NOT NULL DEFAULT 0,
		tokens            BIGINT NOT NULL DEFAULT 0,
		prompt_tokens     BIGINT NOT NULL DEFAULT 0,
		completion_tokens BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS request_daily_model (
		day               TEXT NOT NULL,
		model             TEXT NOT NULL DEFAULT '',
		requests          BIGINT NOT NULL DEFAULT 0,
		successful        BIGINT NOT NULL DEFAULT 0,
		tokens            BIGINT NOT NULL DEFAULT 0,
		prompt_tokens     BIGINT NOT NULL DEFAULT 0,
		completion_tokens BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (day, model)
	)`,
	`CREATE TABLE IF NOT EXISTS request_daily_api (
		day               TEXT NOT NULL,
		api               TEXT NOT NULL DEFAULT '',
		requests          BIGINT NOT NULL DEFAULT 0,
		successful        BIGINT NOT NULL DEFAULT 0,
		tokens            BIGINT NOT NULL DEFAULT 0,
		prompt_tokens     BIGINT NOT NULL DEFAULT 0,
		completion_tokens BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (day, api)
	)`,

	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	)`,

	`CREATE TABLE IF NOT EXISTS runtime_events (
		id         BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		ts         BIGINT NOT NULL,
		level      TEXT NOT NULL DEFAULT 'info',
		scope      TEXT NOT NULL DEFAULT '',
		component  TEXT NOT NULL DEFAULT '',
		account_id TEXT NOT NULL DEFAULT '',
		message    TEXT NOT NULL DEFAULT '',
		detail     TEXT NOT NULL DEFAULT '',
		"count"    BIGINT NOT NULL DEFAULT 1,
		last_ts    BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_event_ts ON runtime_events(ts)`,
	`CREATE INDEX IF NOT EXISTS idx_event_scope ON runtime_events(scope, id)`,
	`CREATE INDEX IF NOT EXISTS idx_event_account ON runtime_events(account_id, id)`,
}

const schemaVersion = 4
