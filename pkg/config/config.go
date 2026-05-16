// Package config loads and exposes application configuration via Viper.
// Priority (high → low): environment variables > config file > defaults.
// Environment variable mapping: use APP_ prefix, e.g. APP_SERVER_PORT=9090.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config is the root configuration structure.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Log       LogConfig       `mapstructure:"log"`
	JWT       JWTConfig       `mapstructure:"jwt"`
	MQ        MQConfig        `mapstructure:"mq"`
	SMS       SMSConfig       `mapstructure:"sms"`       // [Step 3] SMS provider config
	OSS       OSSConfig       `mapstructure:"oss"`       // [Step 9] OSS provider config
	Audit     AuditConfig     `mapstructure:"audit"`     // [Step 10] content moderation config
	Ratelimit RatelimitConfig `mapstructure:"ratelimit"` // [Step 3] generic rate limit knobs
}

type ServerConfig struct {
	Port         int           `mapstructure:"port"`
	Mode         string        `mapstructure:"mode"`          // debug | release | test
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`  // default 10s
	WriteTimeout time.Duration `mapstructure:"write_timeout"` // default 10s
	IdleTimeout  time.Duration `mapstructure:"idle_timeout"`  // default 60s
}

type DatabaseConfig struct {
	DSN          string        `mapstructure:"dsn"`
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
	Provider string        `mapstructure:"provider"` // channel | rabbitmq
	URL      string        `mapstructure:"url"`      // amqp://... when provider=rabbitmq
	Timeout  time.Duration `mapstructure:"timeout"`
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
	Provider        string `mapstructure:"provider"`          // mock | aliyun
	AccessKeyID     string `mapstructure:"access_key_id"`     // env-injected in prod
	AccessKeySecret string `mapstructure:"access_key_secret"` // env-injected in prod
	Region          string `mapstructure:"region"`            // default: cn-shanghai
	MockResult      string `mapstructure:"mock_result"`       // dev only: pass | suspect | reject
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

	// File source.
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("configs")
	v.AddConfigPath(".") // allow running from project root

	// Environment overrides: APP_SERVER_PORT → server.port
	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

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

	v.SetDefault("database.max_open_conns", 25)
	v.SetDefault("database.max_idle_conns", 10)
	v.SetDefault("database.max_lifetime", "5m")
	v.SetDefault("database.max_idle_time", "5m")
	v.SetDefault("database.log_level", "warn")

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
}

func validate(cfg *Config) error {
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port out of range: %d", cfg.Server.Port)
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

	return nil
}
