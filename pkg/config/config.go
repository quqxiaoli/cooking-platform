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
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	Redis    RedisConfig    `mapstructure:"redis"`
	Log      LogConfig      `mapstructure:"log"`
	JWT      JWTConfig      `mapstructure:"jwt"`
	MQ       MQConfig       `mapstructure:"mq"`
}

type ServerConfig struct {
	Port         int           `mapstructure:"port"`
	Mode         string        `mapstructure:"mode"`           // debug | release | test
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`   // default 10s
	WriteTimeout time.Duration `mapstructure:"write_timeout"`  // default 10s
	IdleTimeout  time.Duration `mapstructure:"idle_timeout"`   // default 60s
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
// Provider: "channel" (MVP) | "rabbitmq" (production)
type MQConfig struct {
	Provider string        `mapstructure:"provider"` // channel | rabbitmq
	URL      string        `mapstructure:"url"`      // amqp://... (rabbitmq only)
	Timeout  time.Duration `mapstructure:"timeout"`
}

// Load reads the configuration file and overlays environment variables.
// CONFIG_PATH env var overrides the default file location (configs/config.yaml).
func Load() (*Config, error) {
	v := viper.New()

	// ── file ──────────────────────────────────────────────────────────────────
	configPath := viper.GetString("CONFIG_PATH")
	if configPath == "" {
		configPath = "configs/config.yaml"
	}
	v.SetConfigFile(configPath)

	// ── env vars ──────────────────────────────────────────────────────────────
	// APP_SERVER_PORT=9090 will override server.port
	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// ── defaults ──────────────────────────────────────────────────────────────
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
	v.SetDefault("log.console", false)
	v.SetDefault("log.max_size", 100)
	v.SetDefault("log.max_backups", 7)
	v.SetDefault("log.max_age", 30)
	v.SetDefault("jwt.access_token_ttl", "2h")
	v.SetDefault("jwt.refresh_token_ttl", "168h")
	v.SetDefault("mq.provider", "channel")
	v.SetDefault("mq.timeout", "5s")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %q: %w", configPath, err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	return &cfg, nil
}
