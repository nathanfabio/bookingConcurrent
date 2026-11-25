package booking

import (
	"context"
)

// PaymentFinder is the booking use cases' view of the payments table: the
// single read the confirm gate needs (CLAUDE.md §7's "payment confirmed →
// booking confirmed", anchored per ADR 0006's M5 note and specified in
// ADR 0008).
//
// It deliberately duplicates one method of application/payment.PaymentStore
// instead of importing that package: ports are consumer-owned (ADR 0001),
// and booking's need is one boolean read, not payment's whole vocabulary.
// The Postgres PaymentRepo and the in-memory fake each satisfy BOTH
// interfaces — the same two-consumer shape ScreeningRepo already has with
// booking.ScreeningStore and catalog.ScreeningLister.
type PaymentFinder interface {
	// HasCapturedPayment is the preflight check. The durable booking write
	// independently serializes with refund authorization.
	HasCapturedPayment(ctx context.Context, sessionID string) (bool, error)
}
