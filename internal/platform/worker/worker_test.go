package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/worker"
)

// discardLogger keeps test output clean; the package logs mechanics only.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunStartsAllJobsAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started, finished atomic.Int32

	blockUntilDone := func(ctx context.Context) error {
		started.Add(1)
		<-ctx.Done()
		finished.Add(1)
		return nil
	}

	runReturned := make(chan error, 1)
	go func() {
		runReturned <- worker.Run(ctx, discardLogger(),
			worker.Job{Name: "a", Run: blockUntilDone},
			worker.Job{Name: "b", Run: blockUntilDone},
			worker.Job{Name: "c", Run: blockUntilDone},
		)
	}()

	// All jobs must actually start before anything is canceled.
	deadline := time.After(2 * time.Second)
	for started.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d/3 jobs started", started.Load())
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-runReturned:
		if err != nil {
			t.Errorf("Run after clean cancel = %v, want nil (SIGTERM must exit 0)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if finished.Load() != 3 {
		t.Errorf("finished = %d, want 3", finished.Load())
	}
}

func TestRunJobFailureCancelsSiblings(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("relay exploded")
	siblingSawCancel := make(chan struct{})

	failing := worker.Job{Name: "failing", Run: func(context.Context) error {
		return sentinel
	}}
	sibling := worker.Job{Name: "sibling", Run: func(ctx context.Context) error {
		<-ctx.Done()
		close(siblingSawCancel)
		// A well-behaved job returns nil on cancellation; Run must also
		// tolerate jobs that leak ctx.Err() (normalization path).
		return ctx.Err()
	}}

	err := worker.Run(ctx, discardLogger(), failing, sibling)
	if !errors.Is(err, sentinel) {
		t.Errorf("Run = %v, want it to wrap the sentinel", err)
	}
	select {
	case <-siblingSawCancel:
	case <-time.After(2 * time.Second):
		t.Error("sibling job was not canceled by the failure")
	}
}

func TestTickRunsImmediatelyNotAfterFirstInterval(t *testing.T) {
	// A one-hour interval proves the first run does not wait for a tick:
	// if Tick idled first, firstRun would never fire inside the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	firstRun := make(chan struct{})
	var count atomic.Int32
	err := worker.Tick(ctx, time.Hour, func(context.Context) error {
		if count.Add(1) == 1 {
			close(firstRun)
		}
		return nil
	})
	if err != nil {
		t.Errorf("Tick = %v, want nil after ctx timeout", err)
	}
	select {
	case <-firstRun:
	default:
		t.Error("Tick never ran fn")
	}
	if n := count.Load(); n != 1 {
		t.Errorf("fn ran %d times with a 1h interval, want exactly 1 (the immediate run)", n)
	}
}

func TestTickRepeatsUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var count atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- worker.Tick(ctx, 5*time.Millisecond, func(context.Context) error {
			count.Add(1)
			return nil
		})
	}()

	// Wait for at least a few ticks, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for count.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Tick = %v, want nil after cancel", err)
	}
	if n := count.Load(); n < 3 {
		t.Errorf("fn ran %d times, want >= 3 (ticker must repeat)", n)
	}
}

func TestTickPropagatesFnError(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("store down")
	var count atomic.Int32
	err := worker.Tick(ctx, time.Millisecond, func(context.Context) error {
		if count.Add(1) == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Tick = %v, want the fn error (policy belongs to the caller)", err)
	}
	if n := count.Load(); n != 2 {
		t.Errorf("fn ran %d times, want 2 (Tick must stop at the first error)", n)
	}
}
