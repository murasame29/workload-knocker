// Package logging builds the process-wide slog logger with the stable log schema.
package logging

import (
	"io"
	"log/slog"
)

// ServiceName is the service.name of every log line.
const ServiceName = "workload-knocker"

// ReplaceAttr renames the top-level slog keys time/msg to timestamp/message.
func ReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Key = "timestamp"
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}

// New returns a JSON logger that stamps service.name and service.version on every line.
func New(w io.Writer, level slog.Leveler, version string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: ReplaceAttr})
	return slog.New(h).With(slog.String("service.name", ServiceName), slog.String("service.version", version))
}
