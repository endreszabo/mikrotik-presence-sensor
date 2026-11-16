// Package logging builds the process wide slog.Logger: zorplogger's
// Zorp-style line format by default, or plain JSON.
package logging

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/endreszabo/zorplogger/logfmt"
)

// New returns a logger writing to w in the given format ("zorp" or "json")
// at the given level ("debug", "info", "warn", "error").
func New(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level: %w", err)
	}
	switch format {
	case "zorp", "":
		return slog.New(logfmt.New(w, lvl)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
}

// Component returns a child logger tagged with the subsystem name, which
// zorplogger renders as the "<component>.<severity>" line prefix.
func Component(l *slog.Logger, name string) *slog.Logger {
	return l.With("component", name)
}
