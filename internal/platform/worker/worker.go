// Package worker owns the background-job lifecycle for cmd/worker: the
// supervision mechanics (start jobs, propagate the first failure, shut down
// cleanly on signal) and the ticker loop most jobs run on. It is the
// counterpart of platform/server for the API — same layer, same rules:
// mechanics here, POLICY in the composition root.
//
// The split matters: whether a failing relay tick should kill the process
// (it should not — the outbox buffers, ADR 0009) or a dead consumer
// connection should (it should not — retry with backoff) is error POLICY,
// owned by the job closures in cmd/worker. What lives here is the shared
// MECHANISM: concurrent jobs under one cancellable context, the first real
// failure canceling its siblings, and clean-shutdown normalization so
// SIGTERM exits 0.
//
// Panics are deliberately NOT recovered here: an HTTP request panic is
// recovered per-request so other clients keep being served (CLAUDE.md §5),
// but a panicking background job is a bug with no partial-traffic analogue
// — crashing lets the process supervisor restart the worker with a full
// stack trace, which is the honest failure mode.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

// Job is one supervised background loop. Run blocks until the job is done
// — which for the worker's jobs means until ctx is canceled (clean) or an
// unrecoverable failure occurs (error). Returning nil on ctx cancellation
// is the convention; Run below normalizes stray context.Canceled returns
// anyway, so a job leaking ctx.Err() still exits the process cleanly.
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// Run starts every job concurrently and blocks until ctx is canceled or a
// job fails. The first non-nil error cancels all siblings (errgroup's
// derived context) and is returned wrapped with the job's name; a clean
// ctx cancellation returns nil so the process exits 0 on SIGTERM/SIGINT.
func Run(ctx context.Context, log *slog.Logger, jobs ...Job) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, job := range jobs {
		g.Go(func() error {
			log.Info("worker job started", slog.String("job", job.Name))
			err := job.Run(ctx)
			switch {
			case err == nil:
				log.Info("worker job stopped", slog.String("job", job.Name))
			case errors.Is(err, context.Canceled):
				// Clean shutdown surfaced as ctx.Err() — normalize to nil
				// so it never becomes the errgroup's "first failure".
				log.Info("worker job stopped", slog.String("job", job.Name))
			default:
				log.Error("worker job failed",
					slog.String("job", job.Name),
					slog.String("error", err.Error()))
				return fmt.Errorf("worker: job %s: %w", job.Name, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	log.Info("all worker jobs stopped")
	return nil
}

// Tick runs fn IMMEDIATELY, then on every interval tick, until ctx is
// canceled (returns nil) or fn fails (returns the error — policy lives in
// the caller's fn closure, which may log-and-return-nil to keep ticking).
//
// The immediate first run is deliberate: a worker that just booted (or just
// redeployed) should drain the outbox backlog NOW, not idle for one full
// interval while confirmed bookings wait for their events.
func Tick(ctx context.Context, interval time.Duration, fn func(context.Context) error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := fn(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
