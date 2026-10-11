// Package holdexpiry contains the hold-expiry sweeper use case and the port
// it depends on. Per ADR 0001, the port lives here — next to the code that
// calls it — not in a central ports package and not next to the adapters
// that implement it.
//
// Why this exists (ADR 0002, ADR 0006, ADR 0010): Redis expiry is SILENT.
// When a hold's TTL lapses, the seat and session keys vanish on their own —
// availability stays correct without any help (Postgres-first seat maps,
// TTL-freed seat keys). But the bookkeeping ZSET members (the per-user hold
// set that enforces MAX_ACTIVE_HOLDS, and the global expiry index) linger
// until something ZREMs them, so every silent expiry leaks one of the user's
// hold-limit slots and grows the ZSETs forever.
//
// The sweeper is the reconciler ADR 0002 promised when cmd/worker landed.
// It is deliberately NOT part of booking.HoldStore: the booking service
// never sweeps, and per CLAUDE.md §1 an interface grows the methods its
// CONSUMERS need — this one has its own consumer, two real implementations
// (Redis + the memory fake), and so its own port is discovered, not
// speculative.
package holdexpiry

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// ExpiredHoldStore is the port for reclaiming expired hold state. The Redis
// adapter implements it with a token-checked Lua script (ADR 0002's
// compare-and-delete rule, extended to the sweeper); the memory fake
// implements the same semantics under a mutex.
type ExpiredHoldStore interface {
	// SweepExpired reclaims at most limit holds whose expiry is at or
	// before now (inclusive — the domain treats a hold as expired AT
	// ExpiresAt, and the sweep boundary matches). It returns how many were
	// reclaimed.
	//
	// Contract details the implementations must honor:
	//   - Each member's teardown is ATOMIC and TOKEN-CHECKED: sweeping an
	//     expired hold must never destroy a newer hold of the same seat
	//     (the ADR 0002 guarantee release already provides).
	//   - One corrupt or unparseable member must not fail the batch: skip
	//     it (loudly) and continue.
	//   - now comes from the CALLER: adapters stay clock-free, mirroring
	//     the hold flow where the use case computes ExpiresAt and the
	//     store never runs a second clock.
	SweepExpired(ctx context.Context, now time.Time, limit int) (int, error)
}

// Sweeper runs one bounded sweep per call. Like the outbox Relay, it
// exposes a single-tick method and never owns a timer: the worker drives it
// with platform/worker.Tick, so timing policy stays in the composition
// root and this stays trivially testable.
type Sweeper struct {
	store ExpiredHoldStore
	limit int
	now   func() time.Time
}

// NewSweeper wires the use case. limit comes from validated config
// (WORKER_SWEEP_BATCH_SIZE); now is injected for deterministic tests,
// mirroring payment.NewService and booking.NewService.
func NewSweeper(store ExpiredHoldStore, limit int, now func() time.Time) *Sweeper {
	return &Sweeper{store: store, limit: limit, now: now}
}

// SweepOnce performs ONE bounded sweep pass and reports how many expired
// holds were reclaimed. A non-zero count is logged at info level: that is
// the expired-hold-volume observability ADR 0006 lists as one of the
// sweeper's reasons to exist (a metric graduates it in M8).
func (s *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	swept, err := s.store.SweepExpired(ctx, s.now(), s.limit)
	if err != nil {
		return 0, fmt.Errorf("hold expiry: sweep: %w", err)
	}
	if swept > 0 {
		slog.InfoContext(ctx, "swept expired holds",
			slog.Int("count", swept),
			slog.Int("limit", s.limit))
	}
	return swept, nil
}
