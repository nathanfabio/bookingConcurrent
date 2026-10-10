// Package notification contains the BookingConfirmed consumer use case and
// the ports it depends on. Per ADR 0001, ports live here — next to the code
// that calls them — not in a central ports package and not next to the
// adapters that implement them.
//
// This is CLAUDE.md §6's "at least one consumer reacting to it": the worker
// consumes BookingConfirmed events from the broker and applies an idempotent
// side effect (today a stubbed/logged confirmation, Notifier below). The
// delivery contract is at-least-once (ADR 0009), so duplicates are a normal
// occurrence, not an error: the ProcessedEvents claim table makes the
// EFFECT exactly-once by deduping on the event id.
//
// Like application/payment's WebhookSignatureHeader, the event payload shape
// here is deliberately DUPLICATED from the writer (the bookingConfirmedPayload
// struct in adapters/postgres): consumer-owned contracts beat a shared
// "events" package that couples every producer to every consumer. Both sides
// pin the wire format with reflection tests, and docs/api.md documents it as
// the stable contract between them.
package notification

import (
	"context"
	"time"
)

// EventTypeBookingConfirmed is the event_type value the writer stamps on the
// outbox row and the relay uses as the routing key (ADR 0009's identity
// mapping). Duplicated from the writer's constant on purpose — see the
// package doc.
const EventTypeBookingConfirmed = "BookingConfirmed"

// BookingConfirmed is the decoded event payload. Field names and types MUST
// stay in sync with bookingConfirmedPayload in adapters/postgres/
// repos_booking.go (the writer) — snake_case tags on both sides, pinned by
// reflection tests in both packages. This struct is the consumer's copy of
// the wire contract; adding a field here without the writer emitting it
// yields silent zero values.
type BookingConfirmed struct {
	BookingID   string    `json:"booking_id"`
	SessionID   string    `json:"session_id"`
	ScreeningID string    `json:"screening_id"`
	Row         string    `json:"row"`
	Number      int       `json:"number"`
	UserID      string    `json:"user_id"`
	ConfirmedAt time.Time `json:"confirmed_at"`
}

// ProcessedEvents is the port over the consumer-side dedupe table
// (migrations/00010). The Postgres adapter implements it; the in-memory fake
// backs unit tests.
type ProcessedEvents interface {
	// Claim atomically records that eventID is being processed and reports
	// whether THIS call won the claim: true = first time seen, apply the
	// side effect; false = duplicate delivery, skip it. The check-and-set
	// must be atomic (INSERT .. ON CONFLICT DO NOTHING in Postgres, a
	// mutex-guarded map insert in the fake) so concurrent deliveries of the
	// same event_id produce exactly one winner.
	//
	// Errors are transient (infrastructure) — the caller must NOT treat an
	// errored claim as "duplicate"; it surfaces the error so the broker
	// redelivers.
	Claim(ctx context.Context, eventID, eventType string) (bool, error)
}

// Notifier is the port for the consumer's side effect (CLAUDE.md §6's
// "sends a (stubbed/logged) confirmation email/notification"). M6 ships the
// LogNotifier stub in adapters/notification; a real email/SMS sender is a
// later milestone and MUST bring its own idempotency or invert the handler's
// claim order (ADR 0009 consequence 5).
type Notifier interface {
	// NotifyBookingConfirmed applies the side effect for one decoded event.
	NotifyBookingConfirmed(ctx context.Context, evt BookingConfirmed) error
}
