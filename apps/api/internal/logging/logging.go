// Package logging configures the process-wide structured logger.
//
// The default slog logger is set in [Configure] and is then used everywhere
// via [slog.Default] / [slog.Info] / etc. JSON output is used in production;
// human-readable text output is used in development.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Configure sets [slog.SetDefault] using the given level and format.
//
// level is one of "DEBUG", "INFO", "WARN", "ERROR" (case-insensitive).
// Unrecognised values fall back to INFO.
//
// json=true selects [slog.NewJSONHandler] (suitable for prod / log
// aggregators); json=false selects [slog.NewTextHandler] (suitable for
// local development).
func Configure(level string, json bool) {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if json {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}

func parseLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
