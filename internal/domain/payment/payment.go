// Package payment holds the domain model for payments: the entity, its
// typed status state machine, and the errors the payment use cases surface
// (CLAUDE.md §7).
//
// Everything here is pure Go: no I/O, no infrastructure imports, no
// goroutines. The state machine mirrors — and is intentionally stricter
// than — the payments_status_known CHECK in migration 00005: the CHECK
// keeps bad VALUES out of the table, CanTransitionTo keeps bad SEQUENCES
// out of the flow (e.g. a duplicate webhook flipping captured → captured
// is a no-op handled by the store's guarded UPDATE, not a transition).
package payment

import (
	"errors"
	"time"
)

// Domain errors returned by payment operations. As everywhere in this repo,
// the identity (errors.Is) is what callers branch on; the HTTP layer maps
// these to status codes centrally and the messages are for humans and logs.
var (
	// ErrPaymentNotFound: no payment row exists for the lookup key (intent
	// ID or session). Internally it signals "the webhook names an intent we
	// never minted"; at the API edge it is folded into the same byte-identical
	// 404 as an unknown hold, so payment routes cannot be used to probe
	// session IDs either (CLAUDE.md §3).
	ErrPaymentNotFound = errors.New("payment: not found")
	// ErrPaymentNotCaptured: the session has no captured payment, so the
	// booking cannot be confirmed. This is the gate ADR 0006 asked M5 to
	// anchor confirm to; the HTTP layer answers 402 payment_required.
	ErrPaymentNotCaptured = errors.New("payment: no captured payment for this session")
	// ErrPaymentDeclined: the gateway refused the capture. The intent row is
	// marked failed (a superseding retry can then mint a fresh one — see the
	// payments_active_session_unique partial index, migration 00009).
	ErrPaymentDeclined = errors.New("payment: gateway declined the capture")
	// ErrPaymentInvalidTransition: the requested status change is not legal
	// from the row's current status (e.g. capture on an already-failed
	// intent). A duplicate capture is NOT this error — it is an idempotent
	// no-op, because at-least-once webhooks make duplicates routine.
	ErrPaymentInvalidTransition = errors.New("payment: invalid status transition")
	// ErrPaymentIntentExists: an active (non-failed) intent already exists
	// for the session. An idempotency signal, not a client error: the use
	// case recovers by returning the existing intent (same recovery shape
	// as booking confirm's ErrSessionAlreadyConfirmed, ADR 0006).
	ErrPaymentIntentExists = errors.New("payment: an active intent already exists for this session")
	// ErrPaymentAmountMismatch: the amount in a gateway webhook disagrees
	// with the amount frozen on the intent row. Provider numbers are not
	// trusted blindly (Stripe's own guidance); the payment is marked failed
	// and the mismatch is logged loudly server-side.
	ErrPaymentAmountMismatch = errors.New("payment: webhook amount does not match the frozen intent amount")
	// ErrWebhookInvalid: signature verification failed — bad signature,
	// stale timestamp, malformed header, or unparseable body. Deliberately
	// ONE opaque sentinel: telling a caller which check failed would hand
	// attackers a forgery oracle. Maps to 400 webhook_invalid.
	ErrWebhookInvalid = errors.New("payment: webhook signature verification failed")
)

// Status is the typed state of a payment row. The constants are exactly the
// values migration 00005's CHECK constraint allows; the typed-status
// discipline (type + constants + Valid(), never bare strings) is CLAUDE.md
// §4's rule applied to payments.
type Status string

const (
	// StatusIntent: a payment intent has been minted at the gateway and
	// recorded locally. No money has moved.
	StatusIntent Status = "intent"
	// StatusAuthorized: the gateway has authorized (reserved) the funds but
	// not captured them. Modeled for completeness — the M5 fake gateway
	// captures in one step, so this state is unreachable in current flows
	// (ADR 0008 records the deferral; a Stripe authorize/capture adapter
	// would use it).
	StatusAuthorized Status = "authorized"
	// StatusCaptured: the money has moved. This is the state the booking
	// confirm gate requires (ErrPaymentNotCaptured fires without it).
	StatusCaptured Status = "captured"
	// StatusRefunding: a refund of a captured payment is in flight. Modeled,
	// not reachable in M5 (refunds deferred — ADR 0008, CLAUDE.md §13 "don't
	// gold-plate"). It still blocks new intents for the session on purpose:
	// a refund in flight must not race a fresh charge for the same seat.
	StatusRefunding Status = "refunding"
	// StatusRefunded: the captured money was returned. Terminal for the
	// active-intent index (migration 00009 excludes it), so a refunded
	// session can mint a new intent. Not reachable in M5.
	StatusRefunded Status = "refunded"
	// StatusFailed: the gateway declined, or we failed the row ourselves
	// (e.g. webhook amount mismatch). Terminal — money that actually moved
	// (captured) can only exit via refund, never by "becoming failed", which
	// is why CanTransitionTo forbids captured → failed.
	StatusFailed Status = "failed"
)

// Valid reports whether s is a known payment status.
func (s Status) Valid() bool {
	switch s {
	case StatusIntent, StatusAuthorized, StatusCaptured,
		StatusRefunding, StatusRefunded, StatusFailed:
		return true
	default:
		return false
	}
}

// CanTransitionTo encodes the legal status sequences. It is the state-machine
// half of the migration's promise ("status transitions are enforced in the
// domain"): stores apply guarded UPDATEs whose WHERE clauses express exactly
// these edges, and use cases consult this table before requesting a write.
//
// The edges, and why:
//
//	intent      → authorized | captured | failed   (checkout proceeds or dies)
//	authorized  → captured | failed                (capture or void-by-failure)
//	captured    → refunding                        (money moved: refund is the ONLY exit)
//	refunding   → refunded                         (refund completes)
//	refunded    → (terminal)
//	failed      → (terminal; a retry mints a NEW intent instead)
//
// Same-status "transitions" are false here on purpose: idempotent replays
// (duplicate webhooks) are handled by the store returning the untouched row,
// not by pretending captured → captured is an edge.
func (s Status) CanTransitionTo(next Status) bool {
	switch s {
	case StatusIntent:
		return next == StatusAuthorized || next == StatusCaptured || next == StatusFailed
	case StatusAuthorized:
		return next == StatusCaptured || next == StatusFailed
	case StatusCaptured:
		return next == StatusRefunding
	case StatusRefunding:
		return next == StatusRefunded
	case StatusRefunded, StatusFailed:
		return false // terminal
	default:
		return false // unknown statuses transition nowhere
	}
}

// DefaultCurrency is the only currency M5 prices in. The payments table
// carries a currency column (migration 00005) so multi-currency stays
// representable, but screenings deliberately have no currency field: FX
// semantics are exactly the gold-plating CLAUDE.md §13 forbids (ADR 0008).
const DefaultCurrency = "usd"

// Payment is one recorded payment attempt against a hold session. It stores
// ONLY what CLAUDE.md §7 allows: the gateway's intent ID, our status for
// it, and the frozen amount — never card data, never raw gateway secrets.
//
// BookingSessionID binds the payment to the hold it pays for. IntentID is
// globally unique (payments_intent_unique); at most one ACTIVE (non-failed,
// non-refunded) payment exists per session (payments_active_session_unique,
// migration 00009), so a superseding retry fails the old row first.
type Payment struct {
	ID               string
	BookingSessionID string
	Gateway          string
	IntentID         string
	Status           Status
	AmountCents      int
	Currency         string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// IsCaptured reports whether the money has moved. This is the predicate the
// booking confirm gate is built on (application/booking.PaymentFinder).
func (p Payment) IsCaptured() bool {
	return p.Status == StatusCaptured
}
