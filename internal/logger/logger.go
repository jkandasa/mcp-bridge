// Package logger builds a zap.Logger configured from the log_level string
// in config.yaml and provides a package-level logger for the whole process.
//
// Usage:
//
//	logger.Init("debug")      // once at startup
//	logger.L().Info("ready")  // anywhere in the codebase
package logger

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var global *zap.Logger = zap.NewNop()

// Init builds a production zap logger at the requested level and installs it
// as the package-level logger. Call once from main before starting any
// goroutines that log.
//
// level must be one of: debug, info, warn, error (case-insensitive).
func Init(level string) error {
	lvl, err := parseLevel(level)
	if err != nil {
		return err
	}

	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	// Write logs to stderr so stdout stays clean for any protocol output.
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	// Human-friendly timestamps in ISO-8601.
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	logger, err := cfg.Build(zap.AddCallerSkip(0))
	if err != nil {
		return fmt.Errorf("build zap logger: %w", err)
	}
	global = logger
	return nil
}

// L returns the package-level logger. Always non-nil (no-op before Init).
func L() *zap.Logger { return global }

// Sync flushes any buffered log entries. Call on graceful shutdown.
func Sync() { _ = global.Sync() }

func parseLevel(s string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return zap.DebugLevel, nil
	case "info":
		return zap.InfoLevel, nil
	case "warn":
		return zap.WarnLevel, nil
	case "error":
		return zap.ErrorLevel, nil
	default:
		return zap.InfoLevel, fmt.Errorf("unknown log level %q; must be debug, info, warn, or error", s)
	}
}
