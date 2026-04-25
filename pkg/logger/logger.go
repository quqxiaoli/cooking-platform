// Package logger initialises a global zap.Logger.
// Dev mode (console=true): coloured, human-readable output to stdout.
// Prod mode (console=false): JSON output to rotating file via lumberjack.
package logger

import (
	"os"

	"cooking-platform/pkg/config"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Init constructs and replaces the global zap logger. Call zap.L() anywhere
// in the application after this to get the configured logger.
func Init(cfg config.LogConfig) (*zap.Logger, error) {
	level := parseLevel(cfg.Level)

	encoder := buildEncoder(cfg.Console)
	writer := buildWriter(cfg)

	core := zapcore.NewCore(encoder, writer, level)

	// Always add caller and stack-trace on error+ in production.
	opts := []zap.Option{zap.AddCaller()}
	if !cfg.Console {
		opts = append(opts, zap.AddStacktrace(zapcore.ErrorLevel))
	}

	log := zap.New(core, opts...)

	// Replace the global logger so zap.L() works everywhere.
	zap.ReplaceGlobals(log)

	return log, nil
}

// buildEncoder returns a console encoder for dev and a JSON encoder for prod.
func buildEncoder(console bool) zapcore.Encoder {
	cfg := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	if console {
		cfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		return zapcore.NewConsoleEncoder(cfg)
	}
	return zapcore.NewJSONEncoder(cfg)
}

// buildWriter returns stdout for dev, a rotating file for prod.
func buildWriter(cfg config.LogConfig) zapcore.WriteSyncer {
	if cfg.Console {
		return zapcore.AddSync(os.Stdout)
	}
	filename := cfg.Filename
	if filename == "" {
		filename = "logs/app.log"
	}
	return zapcore.AddSync(&lumberjack.Logger{
		Filename:   filename,
		MaxSize:    cfg.MaxSize,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAge,
		Compress:   cfg.Compress,
		LocalTime:  true,
	})
}

func parseLevel(s string) zapcore.Level {
	switch s {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}
