package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Config holds logging configuration
type Config struct {
	Level  slog.Level
	Format string // "json" or "text"
}

// ConfigFromEnv reads logging configuration from environment variables.
// LOG_LEVEL: debug, info, warn, error (default: info)
// LOG_FORMAT: json, text (default: json)
func ConfigFromEnv() Config {
	cfg := Config{
		Level:  slog.LevelInfo,
		Format: "json",
	}

	if levelStr := os.Getenv("LOG_LEVEL"); levelStr != "" {
		switch strings.ToLower(levelStr) {
		case "debug":
			cfg.Level = slog.LevelDebug
		case "info":
			cfg.Level = slog.LevelInfo
		case "warn", "warning":
			cfg.Level = slog.LevelWarn
		case "error":
			cfg.Level = slog.LevelError
		}
	}

	if format := os.Getenv("LOG_FORMAT"); format != "" {
		switch strings.ToLower(format) {
		case "text":
			cfg.Format = "text"
		case "json":
			cfg.Format = "json"
		}
	}

	return cfg
}

// NewLogger creates a configured slog.Logger
func NewLogger(cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: cfg.Level,
	}

	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// SetDefaultLogger configures the default slog logger from environment
func SetDefaultLogger(cfg Config) {
	logger := NewLogger(cfg)
	slog.SetDefault(logger)
}
