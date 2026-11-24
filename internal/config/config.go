package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是服务的全部运行参数，只从环境变量读。
type Config struct {
	ListenAddr     string
	DataDir        string
	AdminToken     string
	SecretKey      string
	AllowPlaintext bool
	// DatabaseURL 为空时使用 $DataDir/relay.db（SQLite）；
	// 以 postgres:// 或 postgresql:// 开头时连接 Postgres。
	DatabaseURL string
	// 连接池上限，0 表示按方言取默认（SQLite 1/1，Postgres 20/10）。
	DBMaxOpen int
	DBMaxIdle int
	// MigrateSQLite：auto（默认）/ off / require，控制启动时是否把旧 relay.db 导入 Postgres。
	MigrateSQLite string
	// RedisURL 为空时使用进程内实现；有值时为多实例提供分布式锁与共享缓存。
	RedisURL    string
	RedisPrefix string
	// RedisRequired：true 时 Redis 连不上就拒绝启动。
	// 多副本部署建议开启——锁静默失效比启动失败更难排查。
	RedisRequired bool
	// MaxConcurrent 限制同时转发到上游的请求数；超出的请求排队等待，而不是直接拒绝。
	// 0 表示不限制（默认，保持原有行为）。
	MaxConcurrent int
	// QueueWaitSec 是请求最长排队时间，超时返回 503。
	QueueWaitSec int
	PoolStrategy string
	MaxRetries   int
	// RetryUntilExhausted 为 true 时，遇到可换号的错误会按序试完整个号池
	// （直到没有可用账号），而不是提前被 MaxRetries 截断。
	RetryUntilExhausted bool
	// RetryDelayMS 是两次换号之间的等待毫秒数。0 = 不等待。
	RetryDelayMS int
	// 在途租约：开启后同一账号最多同时处理 LeaseConcurrency 个请求，
	// 其余请求换号。关掉则同号并发不限（升级前的行为）。
	LeaseEnabled bool
	// LeaseConcurrency 是单号最多同时持有的租约数，即单号并发上限。
	// 1 = 整号串行（旧行为）；2 = 允许两路并发，以此类推。
	LeaseConcurrency int
	LeaseTTLSec      int
	// 换号退避：失败后在窗口内不再选中该号，时长从 BaseMS 起逐次翻倍、封顶 MaxMS。
	BackoffEnabled bool
	BackoffBaseMS  int
	BackoffMaxMS   int
	// 会话粘性：同一段对话优先复用上次成功的账号。
	AffinityEnabled bool
	AffinityTTLSec  int
	// 无显式会话键时按首条用户消息兜底粘性（见 relay.Engine.stickyKey）。
	AffinityFallback bool
	CheckinEnable    bool
	CheckinHour      int
	CheckinMinute    int
	CheckinJitterMin int
	CheckinGapMinMS  int
	CheckinGapMaxMS  int
	RefreshWindowSec int
	// 主动续期：token 30 天有效，后台提前 RenewWindowSec 秒统一刷新，
	// 避免冷门号因长期不被选中而静默过期。RenewIntervalM 是扫描间隔。
	RenewWindowSec int
	RenewIntervalM int
	RenewGapMS     int
	// StreamIdleTimeout 是对话流「多久没收到上游数据算超时」，不是整条流的总时限。
	// UpstreamTimeoutSec 管登录/余额/目录等非流式端点的整体超时；0 = 用默认 120s。
	StreamIdleTimeout  time.Duration
	UpstreamTimeoutSec int
	MaxBodyMB          int
	ModelCooldown      bool
	// AccountAbnormalCool：上游 200108「账号异常」是否按账号级阶梯冷却。
	// 默认开启；面板可热更新并持久化到 settings。
	AccountAbnormalCool bool
	// 积分阈值：低于 StopBelow 暂停该号，回升到 ResumeAbove 解除。
	CreditStopEnable  bool
	CreditStopBelow   int64
	CreditResumeAbove int64
	// 余额巡检：每隔 BalanceSyncIntervalMin 分钟，把余额过旧的账号（超过
	// BalanceSyncStaleMin 分钟没刷新、或从没读过）挨个查一遍。
	// 0 = 关闭周期巡检，余额只在签到/手动「查余额」时更新。
	// 签到本身会顺手刷一次余额，所以巡检主要补的是「没签到 / 签到没成功 /
	// 日日新没有签到」这三种号。
	BalanceSyncIntervalMin int
	BalanceSyncStaleMin    int
	OAuthRedirectBase      string
	Timezone               string
	UpstreamBase           string

	// 上游客户端指纹：对话/管理面发的版本号与 User-Agent。
	// 上游桌面客户端发版后会过期，面板里可随时修改并持久化到 settings。
	ClientVersion    string
	WebClientVersion string
	OSVersion        string
	DesktopPlatform  string
	ChatUserAgent    string // 留空 = 按 ClientVersion 自动派生
	WebUserAgent     string // 留空 = 按 WebClientVersion 自动派生

	// 日志管理：文件落盘/轮转/过期清理。面板里可覆盖并持久化到 settings 表。
	LogFileEnable     bool
	LogDir            string
	LogMaxSizeMB      int
	LogMaxFiles       int
	LogFileRetainDays int
	LogDBRetainDays   int
	LogCleanEnable    bool
	LogCleanIntervalM int

	// 一键更新：默认从本项目 GitHub Release 拉取构建产物。
	UpdateRepo         string
	GitHubToken        string
	UpdateAPIBase      string
	UpdateDownloadBase string
	UpdateMirrors      string // 空格/逗号分隔的镜像前缀，直连失败时依次兜底
	UpdateTarget       string

	// 出站策略：指纹脱敏与开发者角色兼容。
	// 两者都只改「发给上游的副本」，客户端看到的响应不受影响。
	// 面板「设置 → 网关」里可热更新并持久化到 settings；这里只是首次启动的默认值。
	SanitizeEnabled bool
	// SanitizeProviders 为空 = 应用到全部提供商（含之后新增的）。
	SanitizeProviders []string
	// DeveloperRoleCompat：转发前把 messages 里的 role:"developer" 改写为 "system"。
	DeveloperRoleCompat bool
	// ModelsDevEnabled：用 models.dev 补齐上游没声明的模型能力与思考档位。
	ModelsDevEnabled bool
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func envInt64(key string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// splitList 把空格 / 逗号分隔的串拆成去重后的列表（保留原顺序，统一小写）。
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		v := strings.ToLower(strings.TrimSpace(f))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func envBool(key string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// RandomToken 生成一个 32 字节的随机十六进制串，用于首次启动的 ADMIN_TOKEN。
func RandomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Load 解析环境变量。返回的 Config 已校验完毕，非法组合返回错误而不是静默降级。
func Load() (*Config, error) {
	c := &Config{
		ListenAddr:          env("LISTEN_ADDR", "0.0.0.0:8080"),
		DataDir:             env("DATA_DIR", "/data"),
		AdminToken:          env("ADMIN_TOKEN", ""),
		SecretKey:           env("SECRET_KEY", ""),
		AllowPlaintext:      envBool("ALLOW_PLAINTEXT", false),
		DatabaseURL:         strings.TrimSpace(os.Getenv("DATABASE_URL")),
		DBMaxOpen:           envInt("DB_MAX_OPEN_CONNS", 0),
		DBMaxIdle:           envInt("DB_MAX_IDLE_CONNS", 0),
		MigrateSQLite:       env("MIGRATE_SQLITE", "auto"),
		RedisURL:            strings.TrimSpace(os.Getenv("REDIS_URL")),
		RedisPrefix:         env("REDIS_PREFIX", "raccoon:"),
		RedisRequired:       envBool("REDIS_REQUIRED", false),
		MaxConcurrent:       envInt("MAX_CONCURRENT", 0),
		QueueWaitSec:        envInt("QUEUE_WAIT_SEC", 600),
		PoolStrategy:        env("POOL_STRATEGY", "load_balance"),
		MaxRetries:          envInt("MAX_RETRIES", 3),
		RetryUntilExhausted: envBool("RETRY_UNTIL_EXHAUSTED", false),
		RetryDelayMS:        envInt("RETRY_DELAY_MS", 800),
		// 默认开启租约：同号并发是最容易被上游判成第三方客户端的特征。
		// 默认单号 2 路并发：比整号串行多一倍吞吐，又不至于像完全不限那样明显。
		LeaseEnabled:     envBool("LEASE_ENABLE", true),
		LeaseConcurrency: envInt("LEASE_CONCURRENCY", 2),
		LeaseTTLSec:      envInt("LEASE_TTL_SEC", 300),
		// 退避默认开启、起点很短：只用来错开同一瞬间的密集重试，不是惩罚账号。
		BackoffEnabled:   envBool("BACKOFF_ENABLE", true),
		BackoffBaseMS:    envInt("BACKOFF_BASE_MS", 2000),
		BackoffMaxMS:     envInt("BACKOFF_MAX_MS", 60000),
		AffinityEnabled:  envBool("SESSION_AFFINITY_ENABLE", true),
		AffinityTTLSec:   envInt("SESSION_AFFINITY_TTL_SEC", 86400),
		AffinityFallback: envBool("SESSION_AFFINITY_FALLBACK", true),
		CheckinEnable:    envBool("CHECKIN_ENABLE", true),
		CheckinHour:      envInt("CHECKIN_HOUR", 0),
		CheckinMinute:    envInt("CHECKIN_MINUTE", 10),
		CheckinJitterMin: envInt("CHECKIN_JITTER_MINUTES", 30),
		CheckinGapMinMS:  envInt("CHECKIN_GAP_MIN_MS", 800),
		CheckinGapMaxMS:  envInt("CHECKIN_GAP_MAX_MS", 2000),
		RefreshWindowSec: envInt("REFRESH_WINDOW_SEC", 300),
		// 默认提前 3 天续期、每 6 小时扫一次、账号间隔 1 秒：避开了「提前太久轮换凭据」
		// 的坑，也给长时间停机留够余量。
		RenewWindowSec:      envInt("RENEW_WINDOW_SEC", 3*24*3600),
		RenewIntervalM:      envInt("RENEW_INTERVAL_MIN", 360),
		RenewGapMS:          envInt("RENEW_GAP_MS", 1000),
		StreamIdleTimeout:   time.Duration(envInt("STREAM_IDLE_TIMEOUT_SEC", 120)) * time.Second,
		UpstreamTimeoutSec:  envInt("UPSTREAM_TIMEOUT_SEC", 120),
		MaxBodyMB:           envInt("MAX_BODY_MB", 32),
		ModelCooldown:       envBool("MODEL_COOLDOWN_ENABLE", true),
		AccountAbnormalCool: envBool("ACCOUNT_ABNORMAL_COOL", true),
		CreditStopEnable:    envBool("CREDIT_STOP_ENABLE", false),
		CreditStopBelow:     envInt64("CREDIT_STOP_BELOW", 0),
		CreditResumeAbove:   envInt64("CREDIT_RESUME_ABOVE", 0),
		// 每 10 分钟巡一轮，只查 30 分钟以上没刷新的号：面板上的余额不会长期停在旧值，
		// 又不会让几十个号每分钟都去打上游。0 = 关闭巡检。
		BalanceSyncIntervalMin: envInt("BALANCE_SYNC_INTERVAL_MIN", 10),
		BalanceSyncStaleMin:    envInt("BALANCE_SYNC_STALE_MIN", 30),
		OAuthRedirectBase:      strings.TrimRight(env("OAUTH_REDIRECT_BASE", ""), "/"),
		Timezone:               env("TZ", "Asia/Shanghai"),
		UpstreamBase:           strings.TrimRight(env("UPSTREAM_BASE", "https://xiaohuanxiong.com"), "/"),

		// 指纹默认值与 raccoon 包的出厂常量一致；面板保存后以库里为准。
		ClientVersion:    env("CLIENT_VERSION", "1.0.35"),
		WebClientVersion: env("WEB_CLIENT_VERSION", "v1.1.3"),
		OSVersion:        env("CLIENT_OS_VERSION", "10.0.26200"),
		DesktopPlatform:  env("CLIENT_PLATFORM", "desktop-windows-x64"),
		ChatUserAgent:    env("CLIENT_USER_AGENT", ""),
		WebUserAgent:     env("WEB_USER_AGENT", ""),

		LogFileEnable:     envBool("LOG_FILE_ENABLE", true),
		LogDir:            env("LOG_DIR", filepath.Join(env("DATA_DIR", "/data"), "logs")),
		LogMaxSizeMB:      envInt("LOG_MAX_SIZE_MB", 32),
		LogMaxFiles:       envInt("LOG_MAX_FILES", 10),
		LogFileRetainDays: envInt("LOG_FILE_RETAIN_DAYS", 7),
		LogDBRetainDays:   envInt("LOG_DB_RETAIN_DAYS", 30),
		LogCleanEnable:    envBool("LOG_CLEAN_ENABLE", true),
		LogCleanIntervalM: envInt("LOG_CLEAN_INTERVAL_MINUTES", 60),

		UpdateRepo:         env("UPDATE_REPO", "3166790565/raccoon-relay"),
		GitHubToken:        env("GITHUB_TOKEN", ""),
		UpdateAPIBase:      strings.TrimRight(env("UPDATE_API_BASE", "https://api.github.com"), "/"),
		UpdateDownloadBase: strings.TrimRight(env("UPDATE_DOWNLOAD_BASE", ""), "/"),
		UpdateMirrors:      env("UPDATE_MIRRORS", ""),
		UpdateTarget:       env("UPDATE_TARGET", ""),

		// 出站策略默认值：脱敏与开发者角色兼容默认开（与 Agent2API 取向一致），
		// models.dev 元数据默认开（只补上游没声明的空位，不会推翻上游声明）。
		SanitizeEnabled:     envBool("SANITIZE_ENABLE", true),
		SanitizeProviders:   splitList(env("SANITIZE_PROVIDERS", "")),
		DeveloperRoleCompat: envBool("DEVELOPER_ROLE_COMPAT", true),
		ModelsDevEnabled:    envBool("MODELS_DEV_ENABLE", true),
	}
	if c.AdminToken == "" {
		c.AdminToken = RandomToken()
	}
	if c.SecretKey == "" && !c.AllowPlaintext {
		return nil, fmt.Errorf("未设置 SECRET_KEY：账号 token 将明文落库。确认无妨请显式设置 ALLOW_PLAINTEXT=1")
	}
	// 只接受 postgres 协议：写错成 mysql:// 或 sqlite:// 时要在启动时就拒绝，
	// 而不是等驱动报一个看不懂的错误。
	if c.DatabaseURL != "" {
		lower := strings.ToLower(c.DatabaseURL)
		if !strings.HasPrefix(lower, "postgres://") && !strings.HasPrefix(lower, "postgresql://") {
			return nil, fmt.Errorf("DATABASE_URL 只支持 postgres:// 或 postgresql://，当前值无法识别")
		}
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.CheckinHour < 0 || c.CheckinHour > 23 || c.CheckinMinute < 0 || c.CheckinMinute > 59 {
		return nil, fmt.Errorf("CHECKIN_HOUR/CHECKIN_MINUTE 越界")
	}
	if c.CheckinJitterMin < 0 {
		c.CheckinJitterMin = 0
	}
	if c.CheckinGapMinMS < 0 || c.CheckinGapMaxMS < c.CheckinGapMinMS {
		c.CheckinGapMinMS, c.CheckinGapMaxMS = 800, 2000
	}
	// 旧策略名统一折叠到「负载均衡 / 轮询」两种，非法值退回默认而不是拒绝启动。
	if s, ok := normalizeStrategy(c.PoolStrategy); ok {
		c.PoolStrategy = s
	} else {
		c.PoolStrategy = "load_balance"
	}
	if c.CreditStopBelow < 0 {
		c.CreditStopBelow = 0
	}
	if c.CreditResumeAbove < c.CreditStopBelow {
		c.CreditResumeAbove = c.CreditStopBelow
	}
	if c.BalanceSyncIntervalMin < 0 {
		c.BalanceSyncIntervalMin = 0
	}
	if c.BalanceSyncStaleMin < 0 {
		c.BalanceSyncStaleMin = 0
	}
	// 陈旧窗口必须覆盖住巡检间隔：否则每个号每轮都会被重查一遍，
	// 用户把间隔调小反而制造了密集请求。
	if c.BalanceSyncIntervalMin > 0 && c.BalanceSyncStaleMin < c.BalanceSyncIntervalMin {
		c.BalanceSyncStaleMin = c.BalanceSyncIntervalMin
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("TZ 非法: %s", c.Timezone)
	}
	_ = loc

	// 日志参数收敛：越界值不算致命错误，退回安全默认，避免一个笔误让整个服务起不来。
	if c.LogDir == "" {
		c.LogDir = filepath.Join(c.DataDir, "logs")
	} else if !filepath.IsAbs(c.LogDir) {
		// 相对路径按 DATA_DIR 解析，容器里不会莫名其妙写到工作目录
		c.LogDir = filepath.Join(c.DataDir, c.LogDir)
	}
	if c.LogMaxSizeMB <= 0 {
		c.LogMaxSizeMB = 32
	}
	if c.LogMaxFiles <= 0 {
		c.LogMaxFiles = 10
	}
	if c.LogFileRetainDays < 0 {
		c.LogFileRetainDays = 0
	}
	if c.LogDBRetainDays < 0 {
		c.LogDBRetainDays = 0
	}
	if c.LogCleanIntervalM <= 0 {
		c.LogCleanIntervalM = 60
	}
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("创建 DATA_DIR 失败: %w", err)
	}
	return c, nil
}

// normalizeStrategy 收敛选号策略名。历史策略名（optimal 等）按负载均衡处理。
func normalizeStrategy(s string) (string, bool) {
	switch strings.TrimSpace(s) {
	case "round_robin":
		return "round_robin", true
	case "load_balance", "optimal", "credit_first", "manual", "expire_first":
		return "load_balance", true
	}
	return "", false
}

// Location 返回配置时区，失败回落本地。
func (c *Config) Location() *time.Location {
	if loc, err := time.LoadLocation(c.Timezone); err == nil {
		return loc
	}
	return time.Local
}
