package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup 按配置级别初始化进程默认 slog Logger。
func Setup(level string) *slog.Logger {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: ParseLevel(level),
	}))
	slog.SetDefault(logger)
	return logger
}

// ParseLevel 将配置字符串映射为 slog 级别；未知值回退 info。
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug", "all", "全部":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "fatal":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
