package memory

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
)

// Compile-time proof the fake satisfies BOTH consumer-owned ports over the
// same data — the same two-interface shape the Postgres PaymentRepo has.
var (
	_ apppayment.PaymentStore  = (*PaymentStore)(nil)
	_ appbooking.PaymentFinder = (*PaymentStore)(nil)
)

// PaymentStore is the in-memory fake of the payment store ports.
//
// Faithfulness notes (where it deliberately matches Postgres behavior):
//   - It reproduces the ACTIVE-INTENT ARBITER under the mutex: at most one
//     non-failed, non-refunded payment per session — migration 00009's
//     payments_active_session_unique partial index re-expressed as a scan.
//     A second live intent for one session is ErrPaymentIntentExists, and
//     failing the first makes room for its replacement (supersede =
//     fail-then-insert, exactly like the real schema).
//   - Capture and MarkFailed are guarded transitions mirroring the SQL
//     WHERE status IN ('intent','authorized') guards: replays are
//     idempotent no-ops (capturedNow=false / row returned), terminal and
//     captured rows reject the other edge with ErrPaymentInvalidTransition,
//     and captured → failed is impossible here just like in the database.
//   - InsertIntent assigns identity (ID, timestamps) the way the database
//     does: callers pass a zero ID and get the store's row back. Rows are
//     append-only and ordered by insertion, which stands in for the
//     (created_at DESC, id DESC) ordering of GetActivePaymentBySession.
//
// Concurrency: one mutex guards the row slice, mirroring the atomicity the
// guarded single-statement SQL provides. That is what lets the race tests
// prove the port's exactly-one-transition contract without Postgres.
type PaymentStore struct {
	mu    sync.Mutex
	clock Clock
	rows  []domain.Payment // insertion-ordered, append-only — a table, not a map
}

// NewPaymentStore builds an empty fake on the given clock.
func NewPaymentStore(clock Clock) *PaymentStore {
	return &PaymentStore{clock: clock}
}

// InsertIntent implements payment.PaymentStore.
func (s *PaymentStore) InsertIntent(ctx context.Context, p domain.Payment) (domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.rows {
		if existing.IntentID == p.IntentID {
			// payments_intent_unique. Unreachable with a real gateway
			// (UUID intent IDs); the fake fails loudly like the repo's
			// unrecognized-constraint path rather than pretending.
			return domain.Payment{}, fmt.Errorf("memory: payment insert: duplicate intent id %q", p.IntentID)
		}
		if existing.BookingSessionID == p.BookingSessionID && isActive(existing.Status) {
			// payments_active_session_unique fired.
			return domain.Payment{}, domain.ErrPaymentIntentExists
		}
	}

	// The database assigns identity and timestamps; the query pins status.
	p.ID = uuid.NewString()
	p.Status = domain.StatusIntent
	p.CreatedAt = s.clock.Now()
	p.UpdatedAt = p.CreatedAt

	s.rows = append(s.rows, p)
	return p, nil
}

// GetActiveBySession implements payment.PaymentStore. Latest-active-wins
// mirrors ORDER BY created_at DESC, id DESC LIMIT 1 over the index
// predicate; insertion order is the fake's stand-in for both keys.
func (s *PaymentStore) GetActiveBySession(ctx context.Context, sessionID string) (domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.rows) - 1; i >= 0; i-- {
		if s.rows[i].BookingSessionID == sessionID && isActive(s.rows[i].Status) {
			return s.rows[i], nil
		}
	}
	return domain.Payment{}, domain.ErrPaymentNotFound
}

// GetByIntent implements payment.PaymentStore.
func (s *PaymentStore) GetByIntent(ctx context.Context, intentID string) (domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, row := range s.rows {
		if row.IntentID == intentID {
			return row, nil
		}
	}
	return domain.Payment{}, domain.ErrPaymentNotFound
}

// Capture implements payment.PaymentStore: the guarded edge with the
// three-way classification the repo's UPDATE-then-SELECT performs.
func (s *PaymentStore) Capture(ctx context.Context, intentID string) (domain.Payment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i, err := s.findIndexByIntentLocked(intentID)
	if err != nil {
		return domain.Payment{}, false, err
	}
	switch s.rows[i].Status {
	case domain.StatusIntent, domain.StatusAuthorized:
		s.rows[i].Status = domain.StatusCaptured
		s.rows[i].UpdatedAt = s.clock.Now()
		return s.rows[i], true, nil
	case domain.StatusCaptured:
		return s.rows[i], false, nil // idempotent replay
	default:
		return domain.Payment{}, false, fmt.Errorf(
			"payment: capture: intent %s is in status %q: %w",
			intentID, s.rows[i].Status, domain.ErrPaymentInvalidTransition)
	}
}

// MarkFailed implements payment.PaymentStore with the same guard shape;
// already-failed is the idempotent no-op.
func (s *PaymentStore) MarkFailed(ctx context.Context, intentID string) (domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i, err := s.findIndexByIntentLocked(intentID)
	if err != nil {
		return domain.Payment{}, err
	}
	switch s.rows[i].Status {
	case domain.StatusIntent, domain.StatusAuthorized:
		s.rows[i].Status = domain.StatusFailed
		s.rows[i].UpdatedAt = s.clock.Now()
		return s.rows[i], nil
	case domain.StatusFailed:
		return s.rows[i], nil // idempotent replay
	default:
		return domain.Payment{}, fmt.Errorf(
			"payment: mark failed: intent %s is in status %q: %w",
			intentID, s.rows[i].Status, domain.ErrPaymentInvalidTransition)
	}
}

// HasCapturedPayment implements BOTH appbooking.PaymentFinder and
// apppayment.PaymentStore: the confirm gate's EXISTS read.
func (s *PaymentStore) HasCapturedPayment(ctx context.Context, sessionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, row := range s.rows {
		if row.BookingSessionID == sessionID && row.Status == domain.StatusCaptured {
			return true, nil
		}
	}
	return false, nil
}

// SeedCaptured is a TEST-ONLY shortcut that inserts an already-captured
// payment, mirroring what InsertIntent + Capture produce together. Booking
// confirm tests use it to satisfy the payment gate without replaying the
// whole checkout; production code paths never skip states like this.
func (s *PaymentStore) SeedCaptured(ctx context.Context, sessionID, intentID string, amountCents int) domain.Payment {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	p := domain.Payment{
		ID:               uuid.NewString(),
		BookingSessionID: sessionID,
		Gateway:          "fake",
		IntentID:         intentID,
		Status:           domain.StatusCaptured,
		AmountCents:      amountCents,
		Currency:         domain.DefaultCurrency,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	s.rows = append(s.rows, p)
	return p
}

func (s *PaymentStore) findIndexByIntentLocked(intentID string) (int, error) {
	for i, row := range s.rows {
		if row.IntentID == intentID {
			return i, nil
		}
	}
	return 0, domain.ErrPaymentNotFound
}

// isActive mirrors the payments_active_session_unique predicate
// (migration 00009) — keep the two in sync or the fake stops being an
// arbiter the integration tests agree with.
func isActive(status domain.Status) bool {
	return status != domain.StatusFailed && status != domain.StatusRefunded
}
