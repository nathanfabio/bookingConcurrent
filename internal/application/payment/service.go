package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	domainbooking "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
)

// IntentResult carries what the HTTP layer needs to answer an intent
// creation: the payment row and whether THIS call created it. Created is
// false on an idempotent replay — the session already had an active intent
// and the existing row is returned instead, the same recovery shape
// booking confirm uses (ADR 0006). HoldExpiresAt is the pay-by deadline the
// client should render: after it, capture succeeds but confirm will not
// (ADR 0008's expired-hold hard NO).
type IntentResult struct {
	Payment       domain.Payment
	HoldExpiresAt time.Time
	Created       bool
}

// WebhookOutcome reports what a verified webhook did. Applied distinguishes
// "the event changed our state" from "acknowledged but inert" (duplicate,
// unknown intent, unknown kind, amount mismatch) — the HTTP layer acks both
// with 200, but the difference belongs in logs and future metrics.
type WebhookOutcome struct {
	IntentID string
	Applied  bool
}

// Service orchestrates the payment use cases (CLAUDE.md §7). It depends
// only on ports — PaymentGateway, PaymentStore, HoldFinder, ScreeningPricer
// — so unit tests run against the in-memory fakes and the sandbox gateway,
// and production runs against Postgres + whichever gateway config selects.
//
// now is injected (not time.Now directly) for deterministic expiry tests,
// mirroring application/booking.Service.
type Service struct {
	gateway    PaymentGateway
	store      PaymentStore
	holds      HoldFinder
	screenings ScreeningPricer
	now        func() time.Time
}

// NewService wires the payment use cases. Production passes time.Now for
// the clock; the hold store and screening repo already satisfy the two
// read ports without any adapter changes.
func NewService(gw PaymentGateway, store PaymentStore, holds HoldFinder, screenings ScreeningPricer, now func() time.Time) *Service {
	return &Service{gateway: gw, store: store, holds: holds, screenings: screenings, now: now}
}

// CreateIntent starts checkout for a hold session: validate the caller
// owns a LIVE hold, freeze the screening's price into a gateway intent, and
// record it.
//
// Idempotency: an existing active intent for the session is returned with
// Created=false rather than minting a second charge for one seat. Under a
// race (double-clicked pay button), the payments_active_session_unique
// index arbitrates: the losing insert gets ErrPaymentIntentExists and this
// method recovers by reading the winner back — the loser's gateway-minted
// intent is simply abandoned, which is harmless for the stateless fake and
// becomes a void-it task when a real provider lands (ADR 0008).
func (s *Service) CreateIntent(ctx context.Context, userID, sessionID string) (IntentResult, error) {
	hold, err := s.holdGate(ctx, userID, sessionID)
	if err != nil {
		return IntentResult{}, err // ErrHoldNotFound / ErrNotHoldOwner / ErrHoldExpired
	}

	// Reuse before minting: the common replay never touches the gateway.
	existing, err := s.store.GetActiveBySession(ctx, sessionID)
	if err == nil {
		return IntentResult{Payment: existing, HoldExpiresAt: hold.ExpiresAt, Created: false}, nil
	}
	if !errors.Is(err, domain.ErrPaymentNotFound) {
		return IntentResult{}, fmt.Errorf("payment: create intent: active lookup: %w", err)
	}

	screening, err := s.screenings.Get(ctx, hold.ScreeningID)
	if err != nil {
		// ErrScreeningNotFound is structurally almost impossible (the hold
		// was validated against this screening at creation); anything here
		// is pass-through or infra.
		return IntentResult{}, fmt.Errorf("payment: create intent: screening: %w", err)
	}

	intent, err := s.gateway.CreateIntent(ctx, IntentRequest{
		AmountCents:      screening.PriceCents, // frozen here, never recomputed (ADR 0008)
		Currency:         domain.DefaultCurrency,
		BookingSessionID: sessionID,
	})
	if err != nil {
		return IntentResult{}, fmt.Errorf("payment: create intent: gateway: %w", err)
	}

	row, err := s.store.InsertIntent(ctx, domain.Payment{
		BookingSessionID: sessionID,
		Gateway:          s.gateway.Provider(),
		IntentID:         intent.ID,
		AmountCents:      screening.PriceCents,
		Currency:         domain.DefaultCurrency,
	})
	if errors.Is(err, domain.ErrPaymentIntentExists) {
		// Lost the insert race. The winner's row is the answer; OUR minted
		// intent is abandoned (see the method doc — harmless for the fake).
		winner, getErr := s.store.GetActiveBySession(ctx, sessionID)
		if getErr != nil {
			// The winner vanished between the conflict and the read (it was
			// failed concurrently). Surface the original conflict rather
			// than a confusing ErrPaymentNotFound.
			return IntentResult{}, err
		}
		return IntentResult{Payment: winner, HoldExpiresAt: hold.ExpiresAt, Created: false}, nil
	}
	if err != nil {
		return IntentResult{}, fmt.Errorf("payment: create intent: insert: %w", err)
	}
	return IntentResult{Payment: row, HoldExpiresAt: hold.ExpiresAt, Created: true}, nil
}

// ConfirmByClient is the client-confirm capture path — the sandbox
// equivalent of Stripe's client-side paymentIntent.confirm(), and the dev
// trigger for "the buyer paid". The production-shaped path IS the dev
// fast path; there is no backdoor route (ADR 0008).
//
// Ordering rule: the hold gate runs BEFORE the gateway is called, so our
// own API never charges an expired hold. A tiny window remains — the hold
// can expire between the gate and the capture — and its outcome is honest
// bookkeeping, not lost money: the payment row shows captured, booking
// confirm answers its expired-hold 404, and the refund path (deferred to a
// refunds milestone) is the operational exit.
func (s *Service) ConfirmByClient(ctx context.Context, userID, sessionID string) (domain.Payment, error) {
	if _, err := s.holdGate(ctx, userID, sessionID); err != nil {
		return domain.Payment{}, err
	}

	p, err := s.store.GetActiveBySession(ctx, sessionID)
	if errors.Is(err, domain.ErrPaymentNotFound) {
		// No intent was ever created (or the last one failed and no
		// replacement exists). The client's next step is CreateIntent; the
		// HTTP layer says 402 payment_required.
		return domain.Payment{}, domain.ErrPaymentNotCaptured
	}
	if err != nil {
		return domain.Payment{}, fmt.Errorf("payment: client confirm: active lookup: %w", err)
	}
	if p.IsCaptured() {
		return p, nil // idempotent replay: already paid
	}
	if p.Status != domain.StatusIntent && p.Status != domain.StatusAuthorized {
		// refunding/refunded are unreachable in M5; treat as the invariant
		// break it would be rather than guessing a transition.
		return domain.Payment{}, fmt.Errorf("payment: client confirm: status %q: %w", p.Status, domain.ErrPaymentInvalidTransition)
	}

	if err := s.gateway.CaptureIntent(ctx, p.IntentID, p.AmountCents); err != nil {
		// Declined (or gateway outage). Best-effort mark the row failed so
		// a superseding intent can be minted; if THAT write fails the row
		// stays active and the client's retry hits the capture guard again —
		// never silently pretend the decline was recorded.
		if _, failErr := s.store.MarkFailed(ctx, p.IntentID); failErr != nil {
			slog.ErrorContext(ctx, "payment decline could not be recorded",
				slog.String("intent_id", p.IntentID),
				slog.String("error", failErr.Error()))
		}
		return domain.Payment{}, fmt.Errorf("payment: client confirm: capture: %w: %w", domain.ErrPaymentDeclined, err)
	}

	row, capturedNow, err := s.store.Capture(ctx, p.IntentID)
	if err != nil {
		return domain.Payment{}, fmt.Errorf("payment: client confirm: record capture: %w", err)
	}
	if !capturedNow {
		// A webhook won the race and captured first. Same end state — the
		// money moved exactly once (the guarded UPDATE made that true);
		// this call is the replay.
		slog.InfoContext(ctx, "client confirm replayed a webhook capture",
			slog.String("intent_id", p.IntentID))
	}
	return row, nil
}

// HandleWebhook applies a provider callback. The gateway adapter verified
// the signature over the raw bytes BEFORE anything here runs; this method
// trusts the normalized event's SHAPE but not its CONTENT: the amount is
// cross-checked against the frozen row because provider numbers are input,
// not truth (Stripe's own guidance, ADR 0008).
//
// Ack policy: every signature-VALID event gets a nil error (HTTP 200),
// including duplicates, unknown intents, unknown kinds, and mismatches —
// retrying any of them cannot change the outcome, and non-2xx answers
// invite provider retry storms. Only verification failure (ErrWebhookInvalid)
// and infrastructure errors propagate. Anything inert is logged loudly
// server-side; Applied tells caller and logs which happened.
//
// The webhook path never consults the hold on purpose: it records the
// provider's truth about money that moved. A capture arriving after hold
// expiry is booked as captured, and booking confirm's expiry hard-NO
// (ADR 0006) keeps the seat unsellable-to-this-session — the payment row
// is the audit trail, refunds are the deferred operational exit.
func (s *Service) HandleWebhook(ctx context.Context, rawBody []byte, signatureHeader string) (WebhookOutcome, error) {
	ev, err := s.gateway.VerifyWebhook(rawBody, signatureHeader)
	if err != nil {
		return WebhookOutcome{}, err // ErrWebhookInvalid, opaque by design
	}

	p, err := s.store.GetByIntent(ctx, ev.IntentID)
	if errors.Is(err, domain.ErrPaymentNotFound) {
		// An event for an intent we never minted: forged-but-validly-signed
		// (impossible without the secret), a provider account mix-up, or a
		// row lost to manual surgery. Loud server-side, acked client-side.
		slog.ErrorContext(ctx, "webhook names an unknown payment intent",
			slog.String("intent_id", ev.IntentID),
			slog.String("event_kind", string(ev.Kind)))
		return WebhookOutcome{IntentID: ev.IntentID, Applied: false}, nil
	}
	if err != nil {
		return WebhookOutcome{}, fmt.Errorf("payment: webhook: intent lookup: %w", err)
	}

	switch ev.Kind {
	case EventCaptured:
		return s.applyCaptured(ctx, ev, p)
	case EventFailed:
		row, err := s.store.MarkFailed(ctx, p.IntentID)
		if errors.Is(err, domain.ErrPaymentInvalidTransition) {
			// Already captured (money moved — only a refund can exit) or
			// already terminal. Log the contradiction, ack, change nothing.
			slog.ErrorContext(ctx, "webhook failed-event could not be applied",
				slog.String("intent_id", p.IntentID),
				slog.String("status", string(p.Status)),
				slog.String("error", err.Error()))
			return WebhookOutcome{IntentID: ev.IntentID, Applied: false}, nil
		}
		if err != nil {
			return WebhookOutcome{}, fmt.Errorf("payment: webhook: mark failed: %w", err)
		}
		return WebhookOutcome{IntentID: ev.IntentID, Applied: row.Status == domain.StatusFailed}, nil
	default:
		// Forward-compatible no-op: providers add event kinds; a
		// signature-valid event we do not handle yet is not an error.
		slog.InfoContext(ctx, "webhook delivered an unhandled event kind",
			slog.String("intent_id", ev.IntentID),
			slog.String("event_kind", string(ev.Kind)))
		return WebhookOutcome{IntentID: ev.IntentID, Applied: false}, nil
	}
}

// applyCaptured is the EventCaptured branch: amount cross-check, then the
// guarded capture (duplicate events become store-level no-ops).
func (s *Service) applyCaptured(ctx context.Context, ev WebhookEvent, p domain.Payment) (WebhookOutcome, error) {
	if ev.AmountCents != p.AmountCents {
		// The provider is telling us it moved a different amount than we
		// froze. Do NOT capture. Fail the row (guarded — a captured row
		// stays captured and is only logged), alert loudly, ack so the
		// provider stops retrying an outcome that cannot change.
		slog.ErrorContext(ctx, "webhook amount does not match the frozen intent amount",
			slog.String("intent_id", p.IntentID),
			slog.Int("event_amount_cents", ev.AmountCents),
			slog.Int("frozen_amount_cents", p.AmountCents),
			slog.String("error", domain.ErrPaymentAmountMismatch.Error()))
		if _, err := s.store.MarkFailed(ctx, p.IntentID); err != nil && !errors.Is(err, domain.ErrPaymentInvalidTransition) {
			return WebhookOutcome{}, fmt.Errorf("payment: webhook: fail mismatched intent: %w", err)
		}
		return WebhookOutcome{IntentID: ev.IntentID, Applied: false}, nil
	}

	_, capturedNow, err := s.store.Capture(ctx, p.IntentID)
	if errors.Is(err, domain.ErrPaymentInvalidTransition) {
		// The row failed between the lookup and the guard (a declined
		// client-confirm marked it). The provider says money moved on a
		// dead intent: loud log, ack, ops investigates. Never capture
		// around the guard.
		slog.ErrorContext(ctx, "webhook capture hit a terminal payment row",
			slog.String("intent_id", p.IntentID),
			slog.String("status", string(p.Status)),
			slog.String("error", err.Error()))
		return WebhookOutcome{IntentID: ev.IntentID, Applied: false}, nil
	}
	if err != nil {
		return WebhookOutcome{}, fmt.Errorf("payment: webhook: capture: %w", err)
	}
	return WebhookOutcome{IntentID: ev.IntentID, Applied: capturedNow}, nil
}

// holdGate is the shared entry check for both client-facing payment
// operations: the session must be a LIVE hold owned by this user. It
// surfaces the booking domain's sentinels directly (ErrHoldNotFound,
// ErrNotHoldOwner, ErrHoldExpired) — a use case returning a sibling
// domain's sentinel has precedent (booking.Service passes through
// movie.ErrScreeningNotFound), and the HTTP layer already maps all three
// to the byte-identical 404 that makes session probing useless
// (CLAUDE.md §3). The webhook path deliberately does NOT pass through
// here: provider truth gets recorded regardless of hold state.
func (s *Service) holdGate(ctx context.Context, userID, sessionID string) (*domainbooking.Hold, error) {
	hold, err := s.holds.Get(ctx, sessionID)
	if err != nil {
		return nil, err // ErrHoldNotFound or infra
	}
	if hold.UserID != userID {
		return nil, domainbooking.ErrNotHoldOwner
	}
	if !hold.CanBeConfirmed(s.now()) {
		return nil, domainbooking.ErrHoldExpired
	}
	return hold, nil
}
