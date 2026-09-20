// Package payment contains the payment use cases and the ports they depend
// on. Per ADR 0001, ports live here — next to the code that calls them —
// not in a central ports package and not next to the adapters that
// implement them.
//
// The flows are CLAUDE.md §7's: hold → payment intent → capture (client
// confirm or provider webhook) → the booking confirm gate reads the capture
// (application/booking.PaymentFinder). ADR 0008 records the gateway
// abstraction, the webhook signature scheme, and what M5 defers.
package payment

import (
	"context"

	domainbooking "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
)

// IntentRequest is what the use case hands the gateway to mint a payment
// intent. The amount is already frozen here — read from the screening's
// price at use-case time — so the gateway never decides what to charge.
type IntentRequest struct {
	AmountCents      int
	Currency         string
	BookingSessionID string
}

// Intent is the gateway's answer: the provider-side identifier every later
// operation (capture, webhook correlation) refers to. Deliberately one
// field — the fake has no client_secret to show, and inventing optional
// fields no implementation uses would be speculative port design (ADR 0001).
type Intent struct {
	ID string
}

// EventKind labels a webhook event. Dotted names on purpose: a future
// Stripe adapter maps payment_intent.succeeded → EventCaptured without
// reshaping the port's vocabulary.
type EventKind string

const (
	// EventCaptured: the provider reports the money moved.
	EventCaptured EventKind = "payment.captured"
	// EventFailed: the provider reports the charge died.
	EventFailed EventKind = "payment.failed"
)

// WebhookEvent is a VERIFIED provider notification, normalized to our
// vocabulary. Verification happens inside the gateway adapter (per-provider
// scheme behind the port); by the time a WebhookEvent exists, its signature
// has been checked — but its CONTENT is still provider input and gets
// cross-checked against the frozen row (amount) before it changes anything.
type WebhookEvent struct {
	Kind        EventKind
	IntentID    string
	AmountCents int
}

// PaymentGateway is the port to the payment provider (CLAUDE.md §7). M5
// ships exactly one implementation: the clearly-labeled fake sandbox in
// internal/adapters/payment. Tests and CI use ONLY the fake — a real
// gateway is never hit from automated code. The stripe config slot exists,
// but selecting it fails at boot until the Stripe milestone lands
// (ADR 0008).
type PaymentGateway interface {
	// Provider names the gateway ("fake"). It is stamped into
	// payments.gateway so every row records which implementation minted it
	// — the label travels with the data instead of a config string being
	// threaded through the layers.
	Provider() string

	// CreateIntent asks the provider to reserve a charge of
	// req.AmountCents for the booking session. No money moves yet.
	CreateIntent(ctx context.Context, req IntentRequest) (Intent, error)

	// CaptureIntent tells the provider to collect the reserved funds. It is
	// the client-confirm path's charge step; a decline returns an error and
	// the use case marks the row failed (domain.ErrPaymentDeclined to the
	// caller).
	CaptureIntent(ctx context.Context, intentID string, amountCents int) error

	// VerifyWebhook authenticates a raw provider callback: signature over
	// the EXACT bytes received (rawBody — never a re-encoding), replay
	// tolerance, and payload parsing. Any failure returns
	// domain.ErrWebhookInvalid and nothing else: one opaque error, because
	// telling a forger which check failed is an oracle.
	VerifyWebhook(rawBody []byte, signatureHeader string) (WebhookEvent, error)
}

// PaymentStore is the port for the durable payments table (migration 00005
// + the 00009 active-intent index). The Postgres implementation is the
// arbiter: guarded UPDATEs and the partial unique index make double-capture
// and double-intent impossible under any interleaving (ADR 0008), exactly
// as confirmed_bookings' indexes do for bookings (ADR 0006).
type PaymentStore interface {
	// InsertIntent records a freshly minted intent with status 'intent'.
	//
	// Errors: domain.ErrPaymentIntentExists when the session already has an
	// ACTIVE intent (the arbiter index fired — the use case recovers by
	// reading the winner back, same shape as booking confirm's recovery).
	InsertIntent(ctx context.Context, p domain.Payment) (domain.Payment, error)

	// GetActiveBySession returns the session's live intent (latest
	// non-failed, non-refunded row).
	//
	// Errors: domain.ErrPaymentNotFound when the session has no active
	// payment.
	GetActiveBySession(ctx context.Context, sessionID string) (domain.Payment, error)

	// GetByIntent looks a payment up by the gateway's intent ID — the
	// webhook's entry point.
	//
	// Errors: domain.ErrPaymentNotFound for intents we never minted.
	GetByIntent(ctx context.Context, intentID string) (domain.Payment, error)

	// Capture applies the intent/authorized → captured edge as ONE guarded
	// write. The bool is "transitioned NOW": false with a nil error means
	// the row was ALREADY captured — an idempotent replay (duplicate
	// webhook), which callers must treat as success, not as a no-op to
	// hide. Capture is monotonic in M5 (nothing un-captures), which is what
	// makes the booking confirm gate safe outside a shared transaction
	// (ADR 0008).
	//
	// Errors: domain.ErrPaymentNotFound (unknown intent),
	// domain.ErrPaymentInvalidTransition (the row is failed/refunding/
	// refunded — captured is unreachable from there).
	Capture(ctx context.Context, intentID string) (domain.Payment, bool, error)

	// MarkFailed applies the intent/authorized → failed edge with the same
	// guard shape. Already-failed is an idempotent no-op returning the row;
	// a captured row can never become failed (money that moved exits only
	// via refund), so that case is ErrPaymentInvalidTransition.
	MarkFailed(ctx context.Context, intentID string) (domain.Payment, error)

	// HasCapturedPayment is the booking confirm gate: did the money for
	// this session move? First-captured-wins EXISTS read, mirrored by
	// application/booking.PaymentFinder (same method, second consumer —
	// ADR 0001's "two small interfaces over one store" trade-off).
	HasCapturedPayment(ctx context.Context, sessionID string) (bool, error)
}

// HoldFinder is the payment use cases' view of the hold store: they need to
// load a hold to gate on ownership and liveness, and nothing else. The
// Redis hold store and the in-memory fake already satisfy this shape —
// this interface exists so the dependency stays minimal and explicit
// (consumer-owned ports, ADR 0001).
type HoldFinder interface {
	// Get returns the live hold for sessionID.
	//
	// Errors: domainbooking.ErrHoldNotFound for unknown or expired sessions.
	Get(ctx context.Context, sessionID string) (*domainbooking.Hold, error)
}

// ScreeningPricer is the payment use cases' view of the catalog: the
// ticket price frozen into an intent at creation. The Postgres
// ScreeningRepo and the in-memory fake already satisfy it.
type ScreeningPricer interface {
	// Get loads one screening by id.
	//
	// Errors: domainmovie.ErrScreeningNotFound.
	Get(ctx context.Context, id string) (domainmovie.Screening, error)
}
