// Package logger constructs the process-wide slog.Logger (CLAUDE.md §8).
//
// Format is environment-driven: development gets the human-readable text
// handler; production and test get JSON so log shippers can parse it.
// Level comes from configuration. Request-path logs gain the request ID
// through middleware, not through this package.
package logger

import (
	"context"
	"log/slog"
	"os"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
)

// New builds the process logger.
func New(env config.Environment, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if env == config.EnvDevelopment {
		// Text is for human eyes during `make run`; JSON is for machines.
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

type ctxKey struct{}

// WithContext returns a context carrying l for downstream code that wants
// the request-scoped logger rather than the global one.
func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the request-scoped logger, falling back to the default
// logger. It never returns nil.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
