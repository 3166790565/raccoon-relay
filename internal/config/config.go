package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是服务的全部运行参数，只从环境变量读。
type Config struct {
	ListenAddr        string
	DataDir           string
	AdminToken        string
	SecretKey         string
	AllowPlaintext    bool
	PoolStrategy      string
	MaxRetries        int
	CheckinEnable     bool
	CheckinHour       int
	CheckinMinute     int
	CheckinJitterMin  int
	CheckinGapMinMS   int
	CheckinGapMaxMS   int
	RefreshWindowSec  int
	StreamIdleTimeout time.Duration
	MaxBodyMB         int
	ModelCooldown     bool
	OAuthRedirectBase string
	Timezone          string
	UpstreamBase      string
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
		ListenAddr:        env("LISTEN_ADDR", "0.0.0.0:8080"),
		DataDir:           env("DATA_DIR", "/data"),
		AdminToken:        env("ADMIN_TOKEN", ""),
		SecretKey:         env("SECRET_KEY", ""),
		AllowPlaintext:    envBool("ALLOW_PLAINTEXT", false),
		PoolStrategy:      env("POOL_STRATEGY", "optimal"),
		MaxRetries:        envInt("MAX_RETRIES", 3),
		CheckinEnable:     envBool("CHECKIN_ENABLE", true),
		CheckinHour:       envInt("CHECKIN_HOUR", 0),
		CheckinMinute:     envInt("CHECKIN_MINUTE", 10),
		CheckinJitterMin:  envInt("CHECKIN_JITTER_MINUTES", 30),
		CheckinGapMinMS:   envInt("CHECKIN_GAP_MIN_MS", 800),
		CheckinGapMaxMS:   envInt("CHECKIN_GAP_MAX_MS", 2000),
		RefreshWindowSec:  envInt("REFRESH_WINDOW_SEC", 300),
		StreamIdleTimeout: time.Duration(envInt("STREAM_IDLE_TIMEOUT_SEC", 120)) * time.Second,
		MaxBodyMB:         envInt("MAX_BODY_MB", 32),
		ModelCooldown:     envBool("MODEL_COOLDOWN_ENABLE", true),
		OAuthRedirectBase: strings.TrimRight(env("OAUTH_REDIRECT_BASE", ""), "/"),
		Timezone:          env("TZ", "Asia/Shanghai"),
		UpstreamBase:      strings.TrimRight(env("UPSTREAM_BASE", "https://xiaohuanxiong.com"), "/"),
	}
	if c.AdminToken == "" {
		c.AdminToken = RandomToken()
	}
	if c.SecretKey == "" && !c.AllowPlaintext {
		return nil, fmt.Errorf("未设置 SECRET_KEY：账号 token 将明文落库。确认无妨请显式设置 ALLOW_PLAINTEXT=1")
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
	switch c.PoolStrategy {
	case "optimal", "round_robin", "credit_first", "manual", "expire_first":
	default:
		return nil, fmt.Errorf("POOL_STRATEGY 非法: %s", c.PoolStrategy)
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("TZ 非法: %s", c.Timezone)
	}
	_ = loc
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("创建 DATA_DIR 失败: %w", err)
	}
	return c, nil
}

// Location 返回配置时区，失败回落本地。
func (c *Config) Location() *time.Location {
	if loc, err := time.LoadLocation(c.Timezone); err == nil {
		return loc
	}
	return time.Local
}
