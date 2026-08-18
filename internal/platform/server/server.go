// Package server owns the HTTP server lifecycle: timeouts, and graceful
// shutdown on SIGTERM/SIGINT (CLAUDE.md §5).
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Options configures the server. All timeouts come from validated config —
// no silent zero values, which in net/http mean "no timeout" and are a
// classic slowloris footgun.
type Options struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// Run starts the server and blocks until ctx is cancelled, then drains
// in-flight requests for at most opts.ShutdownTimeout before returning.
// It returns a non-nil error only for real failures (listen errors,
// shutdown deadline exceeded), never for the normal closed path.
//
// Graceful shutdown matters here specifically because an in-flight booking
// request cut mid-write could otherwise leave a client unsure whether a hold
// was created — draining gives those requests their normal completion.
func Run(ctx context.Context, log *slog.Logger, handler http.Handler, opts Options) error {
	srv := &http.Server{
		Addr:         opts.Addr,
		Handler:      handler,
		ReadTimeout:  opts.ReadTimeout,
		WriteTimeout: opts.WriteTimeout,
		// Bridge net/http's own error logging into slog so server-level
		// errors (e.g. handler timeouts) carry our formatting and level rules.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining in-flight requests",
			slog.String("addr", opts.Addr),
			slog.Duration("timeout", opts.ShutdownTimeout))

		shutdownCtx, cancel := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		log.Info("server stopped gracefully")
		return nil
	}
}
