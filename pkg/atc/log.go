package atc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
)

// MultiHandler multiplexes slog records to multiple slog Handlers.
type MultiHandler struct {
	handlers []slog.Handler
}

func NewMultiHandler(handlers ...slog.Handler) slog.Handler {
	return &MultiHandler{handlers: handlers}
}

func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cloned := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		cloned[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: cloned}
}

func (m *MultiHandler) WithGroup(name string) slog.Handler {
	cloned := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		cloned[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: cloned}
}

func initLogger(logFormat string, logLevelStr string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(logLevelStr) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	var baseHandler slog.Handler
	if strings.ToLower(logFormat) == "json" {
		baseHandler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		baseHandler = slog.NewTextHandler(os.Stderr, opts)
	}

	// Create OTel slog handler
	otelHandler := otelslog.NewHandler("atc")

	// Combine base console/file handler with OpenTelemetry handler
	handler := NewMultiHandler(baseHandler, otelHandler)

	return slog.New(handler)
}
