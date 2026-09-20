package http

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
	domainbooking "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
	domainpayment "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/middleware"
)

// webhookSignatureHeader carries the provider's HMAC over the raw request
// body. The name is the wire contract pinned by docs/api.md; the gateway
// adapter (internal/adapters/payment) keeps its own copy of this constant
// because sibling adapters must not import each other (lint-guards Guard 3).
const webhookSignatureHeader = "Payment-Signature"

// Payment DTOs. snake_case JSON is a hard rule (CLAUDE.md §4) — the tag
// test in payment_test.go fails the build on drift.
type (
	// paymentIntentResponse answers POST /holds/{sessionID}/payment-intent.
	// There is NO client_secret field on purpose: the fake gateway has no
	// such artifact, and inventing one would be dishonest API design
	// (ADR 0008). hold_expires_at is the pay-by deadline the client should
	// render — after it, capture still books as captured but confirm
	// refuses the booking (ADR 0006's hard NO).
	paymentIntentResponse struct {
		PaymentID     string    `json:"payment_id"`
		SessionID     string    `json:"session_id"`
		IntentID      string    `json:"intent_id"`
		AmountCents   int       `json:"amount_cents"`
		Currency      string    `json:"currency"`
		Status        string    `json:"status"`
		HoldExpiresAt time.Time `json:"hold_expires_at"`
	}

	// paymentResponse answers the client-confirm capture.
	paymentResponse struct {
		PaymentID   string    `json:"payment_id"`
		IntentID    string    `json:"intent_id"`
		Status      string    `json:"status"`
		AmountCents int       `json:"amount_cents"`
		Currency    string    `json:"currency"`
		UpdatedAt   time.Time `json:"updated_at"`
	}

	// webhookAckResponse is the provider-facing ack. Deliberately minimal:
	// providers only branch on the HTTP status, and echoing event details
	// back would leak our processing state to whoever can hit the endpoint.
	webhookAckResponse struct {
		Received bool `json:"received"`
	}
)

func paymentIntentDTO(res apppayment.IntentResult) paymentIntentResponse {
	return paymentIntentResponse{
		PaymentID:     res.Payment.ID,
		SessionID:     res.Payment.BookingSessionID,
		IntentID:      res.Payment.IntentID,
		AmountCents:   res.Payment.AmountCents,
		Currency:      res.Payment.Currency,
		Status:        string(res.Payment.Status),
		HoldExpiresAt: res.HoldExpiresAt,
	}
}

func paymentDTO(p domainpayment.Payment) paymentResponse {
	return paymentResponse{
		PaymentID:   p.ID,
		IntentID:    p.IntentID,
		Status:      string(p.Status),
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
		UpdatedAt:   p.UpdatedAt,
	}
}

// PaymentIntentHandler implements POST /holds/{sessionID}/payment-intent:
// freeze the screening price into a gateway intent for the caller's live
// hold. 201 the first time; an idempotent replay (the session already has
// an active intent) returns it with 200. Must be wired behind
// middleware.Auth — the user ID comes from the verified token, never the
// body (CLAUDE.md §3), and unknown/foreign/expired sessions all answer the
// byte-identical 404 (no session probing through payment routes either).
func PaymentIntentHandler(svc *apppayment.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.UserIDFromContext(r.Context())
		if !ok {
			// Defense in depth: wiring bug, not a client error.
			WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "authentication required")
			return
		}
		res, err := svc.CreateIntent(r.Context(), userID, r.PathValue("sessionID"))
		if err != nil {
			writePaymentError(w, r, err)
			return
		}
		status := http.StatusCreated
		if !res.Created {
			status = http.StatusOK
		}
		WriteJSON(w, status, paymentIntentDTO(res))
	}
}

// PaymentConfirmHandler implements POST
// /holds/{sessionID}/payment-intent/confirm: the client-confirm capture —
// the sandbox equivalent of Stripe's client-side paymentIntent.confirm()
// and the dev trigger for "the buyer paid" (ADR 0008: the production-shaped
// path IS the dev fast path; there is no backdoor route). Must be wired
// behind middleware.Auth.
func PaymentConfirmHandler(svc *apppayment.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.UserIDFromContext(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "authentication required")
			return
		}
		p, err := svc.ConfirmByClient(r.Context(), userID, r.PathValue("sessionID"))
		if err != nil {
			writePaymentError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, paymentDTO(p))
	}
}

// PaymentWebhookHandler implements POST /webhooks/payment: the provider's
// callback endpoint. PUBLIC — the signature IS its authentication, so it
// must never be wrapped in middleware.Auth (a provider cannot present a
// user JWT). The raw body is read BEFORE anything parses it: the signature
// covers the exact bytes on the wire, and decoding-then-re-encoding would
// verify a different payload than the provider signed.
//
// Ack policy (ADR 0008): every signature-valid event answers 200 —
// including duplicates, unknown intents, and unhandled kinds — because
// retrying them cannot change the outcome and non-2xx invites provider
// retry storms. Any verification failure answers one opaque 400.
func PaymentWebhookHandler(svc *apppayment.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			// Oversized or unreadable body: same opaque 400 as a bad
			// signature — an attacker must not learn WHICH check failed.
			WriteError(w, http.StatusBadRequest, CodeWebhookInvalid, "webhook verification failed")
			return
		}
		outcome, err := svc.HandleWebhook(r.Context(), rawBody, r.Header.Get(webhookSignatureHeader))
		if err != nil {
			writePaymentError(w, r, err)
			return
		}
		if !outcome.Applied {
			// Acked but inert (duplicate, unknown intent, unhandled kind,
			// amount mismatch). The service already logged the detail with
			// the request ID; the response stays minimal.
			slog.WarnContext(r.Context(), "payment webhook acked without applying",
				slog.String("intent_id", outcome.IntentID))
		}
		WriteJSON(w, http.StatusOK, webhookAckResponse{Received: true})
	}
}

// writePaymentError is the sentinel→status mapping for the payment routes,
// mirroring writeBookingError. The three hold sentinels collapse to the
// SAME byte-identical 404 the booking routes use: session probing must not
// succeed just because the URL says "payment" (CLAUDE.md §3).
func writePaymentError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domainbooking.ErrHoldNotFound),
		errors.Is(err, domainbooking.ErrNotHoldOwner),
		errors.Is(err, domainbooking.ErrHoldExpired):
		WriteError(w, http.StatusNotFound, CodeNotFound, "hold not found")
	case errors.Is(err, domainpayment.ErrPaymentNotCaptured):
		WriteError(w, http.StatusPaymentRequired, CodePaymentRequired, "no captured payment for this session")
	case errors.Is(err, domainpayment.ErrPaymentDeclined):
		WriteError(w, http.StatusPaymentRequired, CodePaymentDeclined, "payment was declined")
	case errors.Is(err, domainpayment.ErrWebhookInvalid):
		WriteError(w, http.StatusBadRequest, CodeWebhookInvalid, "webhook verification failed")
	case errors.Is(err, domainmovie.ErrScreeningNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "screening not found")
	default:
		// Includes ErrPaymentInvalidTransition and ErrPaymentAmountMismatch
		// reaching a client-facing path: invariant breaks, not client
		// errors. Log with the request ID, surface nothing specific.
		attrs := []any{slog.String("error", err.Error())}
		if id := middleware.RequestIDFromContext(r.Context()); id != "" {
			attrs = append(attrs, slog.String("request_id", id))
		}
		slog.ErrorContext(r.Context(), "payment request failed", attrs...)
		WriteError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}
