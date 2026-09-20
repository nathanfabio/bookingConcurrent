package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Compile-time proof the repo satisfies BOTH consumer-owned ports over the
// same table (ADR 0001's two-small-interfaces trade-off, same shape as
// ScreeningRepo serving booking.ScreeningStore and catalog.ScreeningLister).
var (
	_ apppayment.PaymentStore  = (*PaymentRepo)(nil)
	_ appbooking.PaymentFinder = (*PaymentRepo)(nil)
)

// Constraint names from migrations/00005 + 00009, pinned for the same
// reason BookingRepo pins its two (see repos_booking.go): the conflict
// mapping below branches on them, integration tests assert them against the
// live schema, and an unrecognized 23505 is logged loudly instead of being
// guessed at.
const (
	constraintPaymentActiveSession = "payments_active_session_unique"
	constraintPaymentIntentUnique  = "payments_intent_unique"
)

// PaymentRepo implements the payment ports against Postgres. It needs no
// pool and no transactions: every operation is ONE statement whose guard
// lives in SQL (the partial unique index for inserts, the status predicate
// for captures) — the schema is the arbiter, so read-modify-write never
// happens and a transaction would add nothing (ADR 0008).
type PaymentRepo struct {
	q *sqlcgen.Queries
}

// NewPaymentRepo builds the repository over the shared pool.
func NewPaymentRepo(pool *pgxpool.Pool) *PaymentRepo {
	return &PaymentRepo{q: sqlcgen.New(pool)}
}

// InsertIntent implements payment.PaymentStore. The status is set by the
// query itself ('intent'); the database assigns id and timestamps, so the
// caller's p only supplies session, gateway, intent ID, and the frozen
// amount/currency.
func (r *PaymentRepo) InsertIntent(ctx context.Context, p domain.Payment) (domain.Payment, error) {
	row, err := r.q.InsertPaymentIntent(ctx, sqlcgen.InsertPaymentIntentParams{
		BookingSessionID: p.BookingSessionID,
		Gateway:          p.Gateway,
		IntentID:         p.IntentID,
		AmountCents:      int32(p.AmountCents),
		Currency:         p.Currency,
	})
	if err != nil {
		if conflict := paymentConflictError(ctx, err); conflict != nil {
			return domain.Payment{}, conflict
		}
		return domain.Payment{}, fmt.Errorf("payment: insert intent: %w", err)
	}
	return paymentFromRow(row), nil
}

// GetActiveBySession implements payment.PaymentStore.
func (r *PaymentRepo) GetActiveBySession(ctx context.Context, sessionID string) (domain.Payment, error) {
	row, err := r.q.GetActivePaymentBySession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, domain.ErrPaymentNotFound
	}
	if err != nil {
		return domain.Payment{}, fmt.Errorf("payment: get active by session: %w", err)
	}
	return paymentFromRow(row), nil
}

// GetByIntent implements payment.PaymentStore.
func (r *PaymentRepo) GetByIntent(ctx context.Context, intentID string) (domain.Payment, error) {
	row, err := r.q.GetPaymentByIntent(ctx, intentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, domain.ErrPaymentNotFound
	}
	if err != nil {
		return domain.Payment{}, fmt.Errorf("payment: get by intent: %w", err)
	}
	return paymentFromRow(row), nil
}

// Capture implements payment.PaymentStore: ONE guarded UPDATE
// (WHERE status IN ('intent','authorized')). A duplicate webhook's UPDATE
// blocks on the row lock, re-evaluates the predicate, matches zero rows —
// double-capture is physically impossible, not merely checked for.
//
// Zero rows is ambiguous by design (unknown intent vs already captured vs
// terminal-other), so the follow-up SELECT classifies it. The two-statement
// sequence needs no transaction because capture is monotonic in M5: a row
// classified as captured stays captured (ADR 0008 — refunds would force
// re-evaluating this).
func (r *PaymentRepo) Capture(ctx context.Context, intentID string) (domain.Payment, bool, error) {
	row, err := r.q.CapturePayment(ctx, intentID)
	if err == nil {
		return paymentFromRow(row), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, false, fmt.Errorf("payment: capture: %w", err)
	}

	// Guard matched nothing. Classify against the row's actual state.
	current, err := r.q.GetPaymentByIntent(ctx, intentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, false, domain.ErrPaymentNotFound
	}
	if err != nil {
		return domain.Payment{}, false, fmt.Errorf("payment: capture: classify: %w", err)
	}
	if domain.Status(current.Status) == domain.StatusCaptured {
		// Idempotent replay: already captured (by us microseconds ago or by
		// a racing webhook). Success, but capturedNow=false so the caller
		// can tell a replay from a first application.
		return paymentFromRow(current), false, nil
	}
	// failed / refunding / refunded: captured is unreachable from here
	// (domain Status.CanTransitionTo says so; the SQL guard enforces it).
	return domain.Payment{}, false, fmt.Errorf(
		"payment: capture: intent %s is in status %q: %w",
		intentID, current.Status, domain.ErrPaymentInvalidTransition)
}

// MarkFailed implements payment.PaymentStore with the same guard shape as
// Capture. Already-failed is a silent no-op returning the row: a gateway
// decline and a "failed" webhook for the same intent must not fight.
func (r *PaymentRepo) MarkFailed(ctx context.Context, intentID string) (domain.Payment, error) {
	row, err := r.q.FailPayment(ctx, intentID)
	if err == nil {
		return paymentFromRow(row), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, fmt.Errorf("payment: mark failed: %w", err)
	}

	current, err := r.q.GetPaymentByIntent(ctx, intentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Payment{}, domain.ErrPaymentNotFound
	}
	if err != nil {
		return domain.Payment{}, fmt.Errorf("payment: mark failed: classify: %w", err)
	}
	if domain.Status(current.Status) == domain.StatusFailed {
		return paymentFromRow(current), nil // idempotent replay
	}
	return domain.Payment{}, fmt.Errorf(
		"payment: mark failed: intent %s is in status %q: %w",
		intentID, current.Status, domain.ErrPaymentInvalidTransition)
}

// HasCapturedPayment implements BOTH appbooking.PaymentFinder and the
// identically-shaped method on apppayment.PaymentStore: the EXISTS read the
// booking confirm gate is built on.
func (r *PaymentRepo) HasCapturedPayment(ctx context.Context, sessionID string) (bool, error) {
	exists, err := r.q.ExistsCapturedPaymentForSession(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("payment: has captured payment: %w", err)
	}
	return exists, nil
}

// paymentConflictError maps a uniqueness violation from the intent insert to
// the domain sentinel for the index that fired, mirroring
// bookingConflictError. Unrecognized constraint on a 23505 → loud log + nil
// so the caller wraps it as a generic failure; guessing would corrupt the
// idempotency signal the service recovers on.
func paymentConflictError(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return nil
	}
	switch pgErr.ConstraintName {
	case constraintPaymentActiveSession:
		return domain.ErrPaymentIntentExists
	case constraintPaymentIntentUnique:
		// A gateway-mint collision is impossible by construction (UUID
		// intent IDs). If it ever fires, the gateway is reusing IDs and
		// that is an operational incident, not a conflict to recover from.
		slog.ErrorContext(ctx, "payment insert hit the intent-id unique index — gateway ID collision",
			slog.String("constraint", pgErr.ConstraintName),
			slog.String("sqlstate", pgErr.Code))
		return nil
	default:
		slog.ErrorContext(ctx, "payment insert hit an unrecognized unique constraint",
			slog.String("constraint", pgErr.ConstraintName),
			slog.String("sqlstate", pgErr.Code))
		return nil
	}
}

func paymentFromRow(row sqlcgen.Payment) domain.Payment {
	return domain.Payment{
		ID:               row.ID,
		BookingSessionID: row.BookingSessionID,
		Gateway:          row.Gateway,
		IntentID:         row.IntentID,
		Status:           domain.Status(row.Status),
		AmountCents:      int(row.AmountCents),
		Currency:         row.Currency,
		CreatedAt:        ts(row.CreatedAt),
		UpdatedAt:        ts(row.UpdatedAt),
	}
}
