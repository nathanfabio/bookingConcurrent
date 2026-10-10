// Package notification holds the production notification adapter: the
// stubbed/logged confirmation sender CLAUDE.md §6 asks for ("a worker binary
// that sends a (stubbed/logged) confirmation email/notification").
//
// This is a real adapter in the production wiring (cmd/worker), not a test
// double — the in-memory Notifier fake in adapters/memory is the test
// double. The stub exists so the whole pipeline (outbox → relay → broker →
// consumer → dedupe → side effect) is exercised end-to-end by a side effect
// that cannot fail, cannot cost money, and cannot spam anyone. A real email/
// SMS sender replaces it behind the same port later and MUST bring its own
// idempotency (ADR 0009 consequence 5: the handler's claim-then-notify
// order is at-most-once for the side effect).
package notification

import (
	"context"
	"log/slog"
	"strconv"

	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"
)

// Compile-time proof the adapter satisfies the port.
var _ appnotification.Notifier = (*LogNotifier)(nil)

// LogNotifier "sends" a booking confirmation by logging it. It never fails:
// every input produces exactly one structured line.
type LogNotifier struct{}

// NewLogNotifier builds the stub.
func NewLogNotifier() *LogNotifier { return &LogNotifier{} }

// NotifyBookingConfirmed implements notification.Notifier. The line is the
// stub's entire side effect — identifiers only, nothing sensitive (the
// log-call guard in scripts/lint-guards.sh and CLAUDE.md §3 apply here as
// everywhere: no user PII beyond ids, never payment data, never credentials).
func (LogNotifier) NotifyBookingConfirmed(ctx context.Context, evt appnotification.BookingConfirmed) error {
	slog.InfoContext(ctx, "booking confirmation notification (stub)",
		slog.String("booking_id", evt.BookingID),
		slog.String("session_id", evt.SessionID),
		slog.String("screening_id", evt.ScreeningID),
		slog.String("seat", evt.Row+strconv.Itoa(evt.Number)),
		slog.String("user_id", evt.UserID),
		slog.Time("confirmed_at", evt.ConfirmedAt),
	)
	return nil
}
