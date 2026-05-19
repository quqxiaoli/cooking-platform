// Package config loads and exposes application configuration via Viper.
// Priority (high → low): environment variables > config file > defaults.
// Environment variable mapping: use APP_ prefix, e.g. APP_SERVER_PORT=9090.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config is the root configuration structure.
type Config struct {
	Server     ServerConfig     `mapstructure:"server"`
	Database   DatabaseConfig   `mapstructure:"database"`
	Redis      RedisConfig      `mapstructure:"redis"`
	Log        LogConfig        `mapstructure:"log"`
	JWT        JWTConfig        `mapstructure:"jwt"`
	MQ         MQConfig         `mapstructure:"mq"`
	SMS        SMSConfig        `mapstructure:"sms"`        // [Step 3] SMS provider config
	OSS        OSSConfig        `mapstructure:"oss"`        // [Step 9] OSS provider config
	Audit      AuditConfig      `mapstructure:"audit"`      // [Step 10] content moderation config
	Encryption EncryptionConfig `mapstructure:"encryption"` // [Step 11] phone field-level encryption
	Ratelimit  RatelimitConfig  `mapstructure:"ratelimit"`  // [Step 3] generic rate limit knobs
	Consumer   ConsumerConfig   `mapstructure:"consumer"`   // [Step 13] per-consumer batch/flush tuning
	Cache      CacheConfig      `mapstructure:"cache"`      // [Step 13] Redis TTL knobs
	Metrics    MetricsConfig    `mapstructure:"metrics"`    // [Step 16] Prometheus metrics
	Search     SearchConfig     `mapstructure:"search"`     // [Step 18] keyword length + boolean operators (was const)
	Follow     FollowConfig     `mapstructure:"follow"`     // [Step 18] follow limits + list sizes (was const)
	CORS       CORSConfig       `mapstructure:"cors"`       // [Step 18] CORS allow lists (was hardcoded "*")
}

type ServerConfig struct {
	Port            int           `mapstructure:"port"`
	Mode            string        `mapstructure:"mode"`             // debug | release | test
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`     // default 10s
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`    // default 10s
	IdleTimeout     time.Duration `mapstructure:"idle_timeout"`     // default 60s
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"` // default 30s — overall LIFO shutdown budget (Step 18)
}

type DatabaseConfig struct {
	DSN          string        `mapstructure:"dsn"`
	SlavesDSN    []string      `mapstructure:"slaves_dsn"`    // [Step 14] replica DSNs for DBResolver; empty → all traffic to master
	MaxOpenConns int           `mapstructure:"max_open_conns"`
	MaxIdleConns int           `mapstructure:"max_idle_conns"`
	MaxLifetime  time.Duration `mapstructure:"max_lifetime"`
	MaxIdleTime  time.Duration `mapstructure:"max_idle_time"`
	LogLevel     string        `mapstructure:"log_level"` // silent | error | warn | info
}

type RedisConfig struct {
	Addr         string        `mapstructure:"addr"`
	Password     string        `mapstructure:"password"`
	DB           int           `mapstructure:"db"`
	PoolSize     int           `mapstructure:"pool_size"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

type LogConfig struct {
	Level      string `mapstructure:"level"`    // debug | info | warn | error
	Console    bool   `mapstructure:"console"`  // true = human-readable for dev
	Filename   string `mapstructure:"filename"` // file path when console=false
	MaxSize    int    `mapstructure:"max_size"` // MB per file
	MaxBackups int    `mapstructure:"max_backups"`
	MaxAge     int    `mapstructure:"max_age"` // days
	Compress   bool   `mapstructure:"compress"`
}

type JWTConfig struct {
	Secret          string        `mapstructure:"secret"`
	AccessTokenTTL  time.Duration `mapstructure:"access_token_ttl"`
	RefreshTokenTTL time.Duration `mapstructure:"refresh_token_ttl"`
}

// MQConfig selects the EventBus implementation.
type MQConfig struct {
	Provider              string        `mapstructure:"provider"`               // channel | rabbitmq
	URL                   string        `mapstructure:"url"`                    // amqp://... when provider=rabbitmq
	Timeout               time.Duration `mapstructure:"timeout"`                // dial timeout
	ReconnectMaxRetries   int           `mapstructure:"reconnect_max_retries"`  // max reconnect attempts (0 = no retry)
	ReconnectInitialDelay time.Duration `mapstructure:"reconnect_initial_delay"` // exponential backoff base; caps at 30s
}

// SMSConfig drives the SMS sender factory.
//
// [Step 3] Provider="mock" uses pkg/sms/mock.go (logs the code instead of sending).
// [Step 10] Provider="aliyun" uses pkg/sms/aliyun.go (real Aliyun dysmsapi).
//
// AccessKeyID / AccessKeySecret are NEVER stored in config.yaml — production
// reads them from APP_SMS_ACCESS_KEY_ID / APP_SMS_ACCESS_KEY_SECRET env vars.
type SMSConfig struct {
	Provider        string        `mapstructure:"provider"`          // mock | aliyun
	AccessKeyID     string        `mapstructure:"access_key_id"`     // env-injected in prod
	AccessKeySecret string        `mapstructure:"access_key_secret"` // env-injected in prod
	SignName        string        `mapstructure:"sign_name"`         // Aliyun signature name
	TemplateCode    string        `mapstructure:"template_code"`     // Aliyun template ID
	CodeLength      int           `mapstructure:"code_length"`       // default 6
	CodeTTL         time.Duration `mapstructure:"code_ttl"`          // default 5m
}

// OSSConfig drives the OSS client factory (added in Step 9).
//
// Provider="mock" spins up a local HTTP listener so verify_step9.sh can run
// the full presign → real PUT → callback chain in dev without Aliyun.
// Provider="aliyun" uses the official SDK and signs PUT URLs directly.
//
// AccessKeyID / AccessKeySecret are NEVER stored in config.yaml — production
// reads them from APP_OSS_ACCESS_KEY_ID / APP_OSS_ACCESS_KEY_SECRET env vars
// injected by docker-compose / Kubernetes secrets.
type OSSConfig struct {
	Provider        string        `mapstructure:"provider"`          // mock | aliyun
	AccessKeyID     string        `mapstructure:"access_key_id"`     // env-injected in prod
	AccessKeySecret string        `mapstructure:"access_key_secret"` // env-injected in prod
	Endpoint        string        `mapstructure:"endpoint"`          // oss-cn-beijing.aliyuncs.com
	Bucket          string        `mapstructure:"bucket"`            // cooking-dev / cooking-prod
	URLPrefix       string        `mapstructure:"url_prefix"`        // baseline for IsAllowedURL whitelist
	PresignTTL      time.Duration `mapstructure:"presign_ttl"`       // default 15m
	MaxImageSize    int64         `mapstructure:"max_image_size"`    // default 5 MiB
	UploadHourly    int           `mapstructure:"upload_hourly"`     // per-user presign rate cap
	MockListenAddr  string        `mapstructure:"mock_listen_addr"`  // dev only, e.g. 127.0.0.1:18080
}

// AuditConfig drives the content moderation auditor factory (added in Step 10).
//
// Provider="mock" returns a MockAuditor whose verdict is set by MockResult
// (pass / suspect / reject). Useful for dev + integration tests.
// Provider="aliyun" calls the Aliyun Green content-safety service synchronously
// inside AuditConsumer — the consumer's goroutine blocks until the API returns.
//
// AccessKeyID / AccessKeySecret: production reads from
// APP_AUDIT_ACCESS_KEY_ID / APP_AUDIT_ACCESS_KEY_SECRET env vars.
type AuditConfig struct {
	Provider        string        `mapstructure:"provider"`          // mock | aliyun
	AccessKeyID     string        `mapstructure:"access_key_id"`     // env-injected in prod
	AccessKeySecret string        `mapstructure:"access_key_secret"` // env-injected in prod
	Region          string        `mapstructure:"region"`            // default: cn-shanghai
	MockResult      string        `mapstructure:"mock_result"`       // dev only: pass | suspect | reject
	Timeout         time.Duration `mapstructure:"timeout"`           // [Step 18] per-call timeout for upstream auditor SDK (AUDIT-01); fields-only today, callers wire in later step
	MaxRetries      int           `mapstructure:"max_retries"`       // [Step 18] retry budget for transient upstream errors
}

// EncryptionConfig holds field-level encryption parameters (added in Step 11).
//
// PhoneKey is a 64-character hex string (= 32 raw bytes, AES-256 key).
// PhonePepper is an arbitrary secret string appended before SHA-256 hashing
// to prevent rainbow-table attacks on the phone_hash column.
//
// Both fields default to empty string. Empty PhoneKey → phone_encrypted stores
// plaintext (dev mode). Empty PhonePepper → hash equals plain SHA-256(phone)
// (backward compatible with Step 3–10 existing rows).
//
// Production reads from env vars:
//
//	APP_ENCRYPTION_PHONE_KEY    — 64 hex chars, required in prod
//	APP_ENCRYPTION_PHONE_PEPPER — any string, required in prod
type EncryptionConfig struct {
	PhoneKey    string `mapstructure:"phone_key"`    // 64 hex chars = 32-byte AES-256 key
	PhonePepper string `mapstructure:"phone_pepper"` // arbitrary secret, pepper for phone_hash
}

// ConsumerConfig holds per-consumer batch/flush tuning knobs (added in Step 13).
// All values were previously hardcoded package-level constants; moving them here
// allows production RabbitMQ tuning without code changes.
type ConsumerConfig struct {
	Like  LikeConsumerConfig  `mapstructure:"like"`
	PV    PVConsumerConfig    `mapstructure:"pv"`
	Count CountConsumerConfig `mapstructure:"count"`
}

type LikeConsumerConfig struct {
	BatchSize     int           `mapstructure:"batch_size"`
	FlushInterval time.Duration `mapstructure:"flush_interval"`
}

type PVConsumerConfig struct {
	BatchSize     int           `mapstructure:"batch_size"`
	FlushInterval time.Duration `mapstructure:"flush_interval"`
}

type CountConsumerConfig struct {
	BatchSize     int           `mapstructure:"batch_size"`
	FlushInterval time.Duration `mapstructure:"flush_interval"`
}

// CacheConfig holds Redis TTLs for the cache layer (added in Step 13).
// Values were previously hardcoded constants in cache/*.go.
type CacheConfig struct {
	LikeStateTTL     time.Duration `mapstructure:"like_state_ttl"`      // like:set:* and like:cnt:* keys
	FeedCacheTTL     time.Duration `mapstructure:"feed_cache_ttl"`      // per-page feed payload cache
	PVDedupTTL       time.Duration `mapstructure:"pv_dedup_ttl"`        // pv:dup:* dedup window
	DedupTTL         time.Duration `mapstructure:"dedup_ttl"`           // [Step 18] dedup:{topic}:{event_id} SETNX window
	UserSMSDailyTTL  time.Duration `mapstructure:"user_sms_daily_ttl"`  // [Step 18] sms:limit:* + sms:ip:* daily counter TTL (USER-03 was 24h const)
}

// SearchConfig holds keyword-sanitisation tunables (Step 18; previously
// package-level consts in internal/service/search_service.go — SEARCH-01).
//
// MaxKeywordLen is a rune count, not byte count, so CJK input gets fair
// budget. BooleanOperators is the literal set of chars stripped before
// running MySQL FULLTEXT BOOLEAN MODE — see search_service.go file header.
type SearchConfig struct {
	MaxKeywordLen    int    `mapstructure:"max_keyword_len"`
	BooleanOperators string `mapstructure:"boolean_operators"`
}

// FollowConfig holds follow-module limits (Step 18; previously
// package-level consts in internal/service/follow_service.go — FOLLOW-01).
type FollowConfig struct {
	MaxFollowing    int `mapstructure:"max_following"`     // per-user follow cap (PRD §8 F-F01 AC-5)
	DefaultListSize int `mapstructure:"default_list_size"` // page size when client omits
	MaxListSize     int `mapstructure:"max_list_size"`     // hard cap on size param
}

// CORSConfig holds the allow-lists used by middleware/cors.go (Step 18;
// previously hardcoded "*" — CORS-01). prod MUST set AllowedOrigins to an
// explicit list; "*" combined with cookies is a CSRF accelerant.
type CORSConfig struct {
	AllowedOrigins []string `mapstructure:"allowed_origins"`
	AllowedMethods []string `mapstructure:"allowed_methods"`
	AllowedHeaders []string `mapstructure:"allowed_headers"`
	ExposeHeaders  []string `mapstructure:"expose_headers"`
}

// MetricsConfig controls Prometheus metric exposition (added in Step 16).
//
// Namespace prefixes all metric names: "cooking" → cooking_http_requests_total.
// When Enabled=false the /metrics route is not registered; no scraping overhead.
type MetricsConfig struct {
	Enabled   bool   `mapstructure:"enabled"`   // default true
	Namespace string `mapstructure:"namespace"` // default "cooking"
}

// RatelimitConfig holds knobs that apply to *generic* rate-limit middleware
// usage across the codebase. SMS-specific three-dimension limits are NOT here
// — they are encoded in user_service because they cannot be expressed as a
// single sliding-window rule.
type RatelimitConfig struct {
	SMSPhoneWindow    time.Duration `mapstructure:"sms_phone_window"`      // 60s
	SMSPerPhonePerDay int           `mapstructure:"sms_per_phone_per_day"` // 5
	SMSPerIPPerDay    int           `mapstructure:"sms_per_ip_per_day"`    // 10
}

// Load reads configuration from configs/config.yaml plus environment variable overrides.
//
// Search order:
//  1. APP_<KEY> environment variables (highest priority)
//  2. configs/config.yaml in working directory
//  3. Hard-coded defaults registered below
func Load() (*Config, error) {
	v := viper.New()

	// File source. CONFIG_PATH env var selects an explicit config file
	// (e.g. Docker deployments use configs/config.docker.yaml to point at
	// internal network hostnames without rebuilding the image).
	// Falls back to the standard configs/config.yaml search path.
	if configPath := os.Getenv("CONFIG_PATH"); configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("configs")
		v.AddConfigPath(".") // allow running from project root
	}

	// Environment overrides: APP_SERVER_PORT → server.port
	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	_ = v.BindEnv("database.dsn", "APP_DATABASE_DSN")

	registerDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		// Missing config file is allowed — defaults + env vars may suffice.
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Viper 已知问题：Unmarshal 对仅通过 BindEnv 绑定、且 yaml 中存在同名 key
	// 的嵌套字段，会优先取 yaml 值而非 env 值。这里在反序列化后手动用
	// v.GetString 强制以 env/BindEnv 解析结果为准（GetString 走的是 Get 路径，
	// BindEnv 在该路径上可靠生效）。
	if dsn := v.GetString("database.dsn"); dsn != "" {
		cfg.Database.DSN = dsn
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

func registerDefaults(v *viper.Viper) {
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.mode", "debug")
	v.SetDefault("server.read_timeout", "10s")
	v.SetDefault("server.write_timeout", "10s")
	v.SetDefault("server.idle_timeout", "60s")
	v.SetDefault("server.shutdown_timeout", "30s") // [Step 18] LIFO shutdown total budget

	v.SetDefault("database.max_open_conns", 25)
	v.SetDefault("database.max_idle_conns", 10)
	v.SetDefault("database.max_lifetime", "5m")
	v.SetDefault("database.max_idle_time", "5m")
	v.SetDefault("database.log_level", "warn")
	v.SetDefault("database.slaves_dsn", []string{}) // [Step 14] empty → single-master mode

	v.SetDefault("redis.pool_size", 10)
	v.SetDefault("redis.dial_timeout", "5s")
	v.SetDefault("redis.read_timeout", "3s")
	v.SetDefault("redis.write_timeout", "3s")

	v.SetDefault("log.level", "info")
	v.SetDefault("log.console", true)
	v.SetDefault("log.filename", "logs/app.log")
	v.SetDefault("log.max_size", 100)
	v.SetDefault("log.max_backups", 7)
	v.SetDefault("log.max_age", 30)
	v.SetDefault("log.compress", true)

	v.SetDefault("jwt.access_token_ttl", "2h")
	v.SetDefault("jwt.refresh_token_ttl", "168h")

	v.SetDefault("mq.provider", "channel")
	v.SetDefault("mq.timeout", "5s")
	v.SetDefault("mq.reconnect_max_retries", 5)
	v.SetDefault("mq.reconnect_initial_delay", "1s")

	// [Step 3] SMS defaults — mock provider, 6-digit codes valid for 5 minutes.
	v.SetDefault("sms.provider", "mock")
	v.SetDefault("sms.code_length", 6)
	v.SetDefault("sms.code_ttl", "5m")

	// [Step 10] Audit defaults — mock provider, auto-pass in dev.
	v.SetDefault("audit.provider", "mock")
	v.SetDefault("audit.region", "cn-shanghai")
	v.SetDefault("audit.mock_result", "pass")

	// [Step 9] OSS defaults — mock provider on 127.0.0.1:18080.
	v.SetDefault("oss.provider", "mock")
	v.SetDefault("oss.presign_ttl", "15m")
	v.SetDefault("oss.max_image_size", 5*1024*1024)
	v.SetDefault("oss.upload_hourly", 30)
	v.SetDefault("oss.mock_listen_addr", "127.0.0.1:18080")
	v.SetDefault("oss.bucket", "cooking-dev")
	v.SetDefault("oss.url_prefix", "http://127.0.0.1:18080/")

	// [Step 3] Rate-limit defaults — three-dimension SMS protection.
	v.SetDefault("ratelimit.sms_phone_window", "60s")
	v.SetDefault("ratelimit.sms_per_phone_per_day", 5)
	v.SetDefault("ratelimit.sms_per_ip_per_day", 10)

	// [Step 13] Consumer batch/flush defaults — previously hardcoded constants.
	v.SetDefault("consumer.like.batch_size", 50)
	v.SetDefault("consumer.like.flush_interval", "3s")
	v.SetDefault("consumer.pv.batch_size", 100)
	v.SetDefault("consumer.pv.flush_interval", "5s")
	v.SetDefault("consumer.count.batch_size", 20)
	v.SetDefault("consumer.count.flush_interval", "10s")

	// [Step 13] Cache TTL defaults — previously hardcoded constants.
	v.SetDefault("cache.like_state_ttl", "168h") // 7 days
	v.SetDefault("cache.feed_cache_ttl", "5m")
	v.SetDefault("cache.pv_dedup_ttl", "1h")
	v.SetDefault("cache.dedup_ttl", "24h")            // [Step 18] event-id SETNX dedup
	v.SetDefault("cache.user_sms_daily_ttl", "24h")   // [Step 18] USER-03 const → cfg

	// [Step 18] Search / Follow / CORS / Audit defaults — Config-First补丁.
	v.SetDefault("search.max_keyword_len", 50)
	v.SetDefault("search.boolean_operators", `+-><()~*"@`)
	v.SetDefault("follow.max_following", 3000)
	v.SetDefault("follow.default_list_size", 20)
	v.SetDefault("follow.max_list_size", 50)
	v.SetDefault("cors.allowed_origins", []string{"*"}) // dev permissive; prod overrides
	v.SetDefault("cors.allowed_methods", []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"})
	v.SetDefault("cors.allowed_headers", []string{"Origin", "Content-Type", "Authorization", "X-Request-ID"})
	v.SetDefault("cors.expose_headers", []string{"X-Request-ID"})
	v.SetDefault("audit.timeout", "3s")
	v.SetDefault("audit.max_retries", 2)

	// [Step 16] Metrics defaults.
	v.SetDefault("metrics.enabled", true)
	v.SetDefault("metrics.namespace", "cooking")
}

func validate(cfg *Config) error {
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port out of range: %d", cfg.Server.Port)
	}
	if cfg.Server.ShutdownTimeout <= 0 {
		return fmt.Errorf("server.shutdown_timeout must be positive")
	}
	if cfg.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if cfg.Redis.Addr == "" {
		return fmt.Errorf("redis.addr is required")
	}
	if len(cfg.JWT.Secret) < 32 {
		return fmt.Errorf("jwt.secret must be at least 32 characters")
	}
	if cfg.JWT.AccessTokenTTL <= 0 {
		return fmt.Errorf("jwt.access_token_ttl must be positive")
	}
	if cfg.JWT.RefreshTokenTTL <= cfg.JWT.AccessTokenTTL {
		return fmt.Errorf("jwt.refresh_token_ttl must be greater than access_token_ttl")
	}

	switch cfg.MQ.Provider {
	case "channel", "rabbitmq", "":
	default:
		return fmt.Errorf("mq.provider must be 'channel' or 'rabbitmq', got %q", cfg.MQ.Provider)
	}
	if cfg.MQ.ReconnectMaxRetries < 0 {
		return fmt.Errorf("mq.reconnect_max_retries must be >= 0, got %d", cfg.MQ.ReconnectMaxRetries)
	}
	if cfg.MQ.ReconnectInitialDelay <= 0 {
		return fmt.Errorf("mq.reconnect_initial_delay must be positive")
	}

	switch cfg.SMS.Provider {
	case "mock", "aliyun", "":
	default:
		return fmt.Errorf("sms.provider must be 'mock' or 'aliyun', got %q", cfg.SMS.Provider)
	}
	if cfg.SMS.CodeLength < 4 || cfg.SMS.CodeLength > 8 {
		return fmt.Errorf("sms.code_length must be between 4 and 8, got %d", cfg.SMS.CodeLength)
	}
	if cfg.SMS.CodeTTL <= 0 {
		return fmt.Errorf("sms.code_ttl must be positive")
	}
	if cfg.SMS.Provider == "aliyun" {
		if cfg.SMS.AccessKeyID == "" || cfg.SMS.AccessKeySecret == "" {
			return fmt.Errorf("sms.access_key_id and sms.access_key_secret are required when provider=aliyun")
		}
		if cfg.SMS.SignName == "" || cfg.SMS.TemplateCode == "" {
			return fmt.Errorf("sms.sign_name and sms.template_code are required when provider=aliyun")
		}
	}

	switch cfg.Audit.Provider {
	case "mock", "aliyun", "":
	default:
		return fmt.Errorf("audit.provider must be 'mock' or 'aliyun', got %q", cfg.Audit.Provider)
	}
	if cfg.Audit.Provider == "aliyun" {
		if cfg.Audit.AccessKeyID == "" || cfg.Audit.AccessKeySecret == "" {
			return fmt.Errorf("audit.access_key_id and audit.access_key_secret are required when provider=aliyun")
		}
	}
	switch cfg.Audit.MockResult {
	case "pass", "suspect", "reject", "":
	default:
		return fmt.Errorf("audit.mock_result must be 'pass', 'suspect', or 'reject', got %q", cfg.Audit.MockResult)
	}

	// [Step 9] OSS validation.
	switch cfg.OSS.Provider {
	case "mock", "aliyun", "":
	default:
		return fmt.Errorf("oss.provider must be 'mock' or 'aliyun', got %q", cfg.OSS.Provider)
	}
	if cfg.OSS.URLPrefix == "" {
		return fmt.Errorf("oss.url_prefix is required (whitelist baseline)")
	}
	if cfg.OSS.URLPrefix != strings.ToLower(cfg.OSS.URLPrefix) {
		// IsAllowedURL lowercases both sides; configured prefix must be
		// lower-case so that the comparison stays a literal prefix match
		// for prod-deployment audits.
		return fmt.Errorf("oss.url_prefix must be lower case, got %q", cfg.OSS.URLPrefix)
	}
	if cfg.OSS.PresignTTL <= 0 {
		return fmt.Errorf("oss.presign_ttl must be positive")
	}
	if cfg.OSS.MaxImageSize <= 0 {
		return fmt.Errorf("oss.max_image_size must be positive")
	}
	if cfg.OSS.UploadHourly <= 0 {
		return fmt.Errorf("oss.upload_hourly must be positive")
	}
	if cfg.OSS.Provider == "aliyun" {
		if cfg.OSS.AccessKeyID == "" || cfg.OSS.AccessKeySecret == "" {
			return fmt.Errorf("oss.access_key_id and oss.access_key_secret are required when provider=aliyun")
		}
		if cfg.OSS.Endpoint == "" {
			return fmt.Errorf("oss.endpoint is required when provider=aliyun")
		}
		if cfg.OSS.Bucket == "" {
			return fmt.Errorf("oss.bucket is required when provider=aliyun")
		}
	}
	if cfg.OSS.Provider == "mock" && cfg.OSS.MockListenAddr == "" {
		return fmt.Errorf("oss.mock_listen_addr is required when provider=mock")
	}

	if cfg.Ratelimit.SMSPhoneWindow <= 0 {
		return fmt.Errorf("ratelimit.sms_phone_window must be positive")
	}
	if cfg.Ratelimit.SMSPerPhonePerDay <= 0 {
		return fmt.Errorf("ratelimit.sms_per_phone_per_day must be positive")
	}
	if cfg.Ratelimit.SMSPerIPPerDay <= 0 {
		return fmt.Errorf("ratelimit.sms_per_ip_per_day must be positive")
	}

	// [Step 13] Consumer config validation.
	if cfg.Consumer.Like.BatchSize <= 0 {
		return fmt.Errorf("consumer.like.batch_size must be positive")
	}
	if cfg.Consumer.Like.FlushInterval <= 0 {
		return fmt.Errorf("consumer.like.flush_interval must be positive")
	}
	if cfg.Consumer.PV.BatchSize <= 0 {
		return fmt.Errorf("consumer.pv.batch_size must be positive")
	}
	if cfg.Consumer.PV.FlushInterval <= 0 {
		return fmt.Errorf("consumer.pv.flush_interval must be positive")
	}
	if cfg.Consumer.Count.BatchSize <= 0 {
		return fmt.Errorf("consumer.count.batch_size must be positive")
	}
	if cfg.Consumer.Count.FlushInterval <= 0 {
		return fmt.Errorf("consumer.count.flush_interval must be positive")
	}

	// [Step 13] Cache TTL validation.
	if cfg.Cache.LikeStateTTL <= 0 {
		return fmt.Errorf("cache.like_state_ttl must be positive")
	}
	if cfg.Cache.FeedCacheTTL <= 0 {
		return fmt.Errorf("cache.feed_cache_ttl must be positive")
	}
	if cfg.Cache.PVDedupTTL <= 0 {
		return fmt.Errorf("cache.pv_dedup_ttl must be positive")
	}
	if cfg.Cache.DedupTTL <= 0 {
		return fmt.Errorf("cache.dedup_ttl must be positive")
	}
	if cfg.Cache.UserSMSDailyTTL <= 0 {
		return fmt.Errorf("cache.user_sms_daily_ttl must be positive")
	}

	// [Step 18] Search / Follow / CORS validation.
	if cfg.Search.MaxKeywordLen <= 0 {
		return fmt.Errorf("search.max_keyword_len must be positive")
	}
	if cfg.Search.BooleanOperators == "" {
		return fmt.Errorf("search.boolean_operators must be non-empty")
	}
	if cfg.Follow.MaxFollowing <= 0 {
		return fmt.Errorf("follow.max_following must be positive")
	}
	if cfg.Follow.DefaultListSize <= 0 || cfg.Follow.DefaultListSize > cfg.Follow.MaxListSize {
		return fmt.Errorf("follow.default_list_size must be in (0, max_list_size]")
	}
	if cfg.Follow.MaxListSize <= 0 {
		return fmt.Errorf("follow.max_list_size must be positive")
	}
	if len(cfg.CORS.AllowedOrigins) == 0 {
		return fmt.Errorf("cors.allowed_origins must be non-empty (use [\"*\"] for dev permissive)")
	}
	if cfg.Server.Mode == "release" {
		for _, origin := range cfg.CORS.AllowedOrigins {
			if origin == "*" {
				return fmt.Errorf("cors.allowed_origins must not contain \"*\" in release mode (CSRF risk)")
			}
		}
	}
	if cfg.Audit.Timeout <= 0 {
		return fmt.Errorf("audit.timeout must be positive")
	}
	if cfg.Audit.MaxRetries < 0 {
		return fmt.Errorf("audit.max_retries must be >= 0")
	}

	// [Step 16] Metrics validation — namespace must be non-empty when enabled
	// so Prometheus scraping never sees nameless metrics (would collide with
	// other services on the same Prometheus instance).
	if cfg.Metrics.Enabled && cfg.Metrics.Namespace == "" {
		return fmt.Errorf("metrics.namespace must be non-empty when metrics.enabled=true")
	}

	// [Step 11] Encryption validation.
	// [Step 18] GUARD-18: release mode MUST have a non-empty phone_key; this
	// pairs with pkg/crypto.ErrEmptyKey fail-closed semantics so that a
	// misconfigured prod deployment crashes at boot instead of silently
	// writing plaintext phones to users.phone_encrypted.
	if cfg.Server.Mode == "release" && cfg.Encryption.PhoneKey == "" {
		return fmt.Errorf("encryption.phone_key must be set in release mode (inject APP_ENCRYPTION_PHONE_KEY)")
	}
	if key := cfg.Encryption.PhoneKey; key != "" {
		if len(key) != 64 {
			return fmt.Errorf("encryption.phone_key must be 64 hex characters (32 bytes), got %d chars", len(key))
		}
		for i, c := range key {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return fmt.Errorf("encryption.phone_key contains non-hex character %q at position %d", c, i)
			}
		}
	}

	return nil
}
