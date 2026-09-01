package booking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
)

// ConfirmResult carries what the HTTP layer needs to answer a confirm: the
// booking and whether THIS call created it. Created is false on an
// idempotent replay — the session had already confirmed, and the recovered
// row is returned instead (ADR 0006). The handler turns that into 201 vs
// 200.
type ConfirmResult struct {
	Booking domain.Booking
	Created bool
}

// SeatState pairs one seat of the screening grid with its computed status.
type SeatState struct {
	Seat   domain.Seat
	Status domain.SeatStatus
}

// SeatMapView is the full, ready-to-render seat map (CLAUDE.md §4): the
// screening geometry plus one status per seat, computed server-side with
// Postgres-first precedence (ADR 0006).
type SeatMapView struct {
	Screening domainmovie.Screening
	Seats     []SeatState
}

// Service orchestrates the booking use cases. It depends only on ports —
// HoldStore, BookingStore, ScreeningStore — so unit tests run against the
// in-memory fakes and production runs against Redis + Postgres.
//
// now is injected (not time.Now directly) so expiry-dependent behavior is
// testable deterministically, mirroring the memory.Clock idea without
// importing an adapter into the application layer.
type Service struct {
	holds      HoldStore
	bookings   BookingStore
	screenings ScreeningStore
	holdTTL    time.Duration
	now        func() time.Time
}

// NewService wires the booking use cases. holdTTL comes from validated
// config; production passes time.Now for the clock.
func NewService(holds HoldStore, bookings BookingStore, screenings ScreeningStore, holdTTL time.Duration, now func() time.Time) *Service {
	return &Service{
		holds:      holds,
		bookings:   bookings,
		screenings: screenings,
		holdTTL:    holdTTL,
		now:        now,
	}
}

// Hold claims a seat for checkout (CLAUDE.md §4). Every rejection happens
// BEFORE Redis is touched: unknown screening, seat outside the geometry, or
// seat already sold. Only a fully validated claim reaches the hold store —
// that ordering is the difference between "Redis as lock service" and
// "Redis as validated fast path".
//
// The SeatConfirmed pre-check is the phantom-hold defense: it stops the
// common case (holding an already-booked seat) with one indexed lookup, but
// it is NOT the arbiter — a seat can be confirmed between the check and the
// claim, and the partial unique index re-checks at confirm time (ADR 0006).
func (s *Service) Hold(ctx context.Context, userID, screeningID string, seat domain.Seat) (domain.Hold, error) {
	screening, err := s.screenings.Get(ctx, screeningID)
	if err != nil {
		return domain.Hold{}, err // ErrScreeningNotFound or infra
	}
	if !screening.HasSeat(seat) {
		return domain.Hold{}, domain.ErrSeatOutOfRange
	}

	confirmed, err := s.bookings.SeatConfirmed(ctx, screeningID, seat)
	if err != nil {
		return domain.Hold{}, fmt.Errorf("booking: hold: seat check: %w", err)
	}
	if confirmed {
		return domain.Hold{}, domain.ErrSeatAlreadyBooked
	}

	now := s.now()
	hold := domain.Hold{
		SessionID:   uuid.NewString(),
		ScreeningID: screeningID,
		Seat:        seat,
		UserID:      userID,
		HoldToken:   uuid.NewString(),
		ExpiresAt:   now.Add(s.holdTTL),
	}
	// Defense in depth: a malformed hold (bad TTL arithmetic, empty fields)
	// must die here, not inside the store.
	if err := hold.Validate(now); err != nil {
		return domain.Hold{}, fmt.Errorf("booking: hold: %w", err)
	}
	if err := s.holds.Hold(ctx, hold); err != nil {
		return domain.Hold{}, err // ErrSeatAlreadyHeld, ErrHoldLimitExceeded, infra
	}
	return hold, nil
}

// Confirm turns a live hold into the durable sales record. The consistency
// model is ADR 0006: the Postgres transaction is the commit point; Redis
// cleanup is best-effort AFTER the commit, because releasing the hold first
// would strand the buyer if the durable write then failed.
func (s *Service) Confirm(ctx context.Context, userID, sessionID string) (ConfirmResult, error) {
	hold, err := s.holds.Get(ctx, sessionID)
	if err != nil {
		return ConfirmResult{}, err // ErrHoldNotFound or infra
	}
	if hold.UserID != userID {
		// Someone else's session. Mapped to the same 404 as an unknown
		// session (CLAUDE.md §3): probing session IDs must not reveal which
		// ones exist for other users.
		return ConfirmResult{}, domain.ErrNotHoldOwner
	}
	if !hold.CanBeConfirmed(s.now()) {
		// The sub-second window where the store still serves the hold but
		// the business rule says no. A hard NO even with payment authorized
		// (ADR 0006); M5 re-anchors this to payment capture.
		return ConfirmResult{}, domain.ErrHoldExpired
	}

	b := domain.Booking{
		SessionID:   hold.SessionID,
		ScreeningID: hold.ScreeningID,
		Seat:        hold.Seat,
		UserID:      hold.UserID,
		Status:      domain.StatusConfirmed,
	}
	created, err := s.bookings.Confirm(ctx, b)
	if err == nil {
		s.cleanupHold(ctx, sessionID, userID)
		return ConfirmResult{Booking: created, Created: true}, nil
	}

	// Conflict. Either sentinel can fire on an idempotent replay: the insert
	// duplicates BOTH the session row and the confirmed seat row, and which
	// unique index Postgres reports is not guaranteed. So recovery runs for
	// both (ADR 0006).
	if errors.Is(err, domain.ErrSeatAlreadyBooked) || errors.Is(err, domain.ErrSessionAlreadyConfirmed) {
		return s.recoverConflict(ctx, err, userID, sessionID)
	}
	return ConfirmResult{}, err // infra
}

// recoverConflict distinguishes "I already confirmed this" from "someone
// else won the seat" by looking up the session's own booking row.
func (s *Service) recoverConflict(ctx context.Context, conflict error, userID, sessionID string) (ConfirmResult, error) {
	existing, err := s.bookings.GetBySession(ctx, sessionID)
	if errors.Is(err, domain.ErrBookingNotFound) {
		// No row for this session, so the conflict came from the seat
		// index: someone else's booking owns it. Surface the original error.
		return ConfirmResult{}, conflict
	}
	if err != nil {
		return ConfirmResult{}, fmt.Errorf("booking: confirm: conflict recovery: %w", err)
	}
	if existing.UserID != userID {
		// Impossible by construction: session IDs are per-hold random UUIDs
		// minted for one user, so a foreign row here means something is
		// deeply wrong. Never leak that to the client — log and 500.
		slog.ErrorContext(ctx, "confirm conflict recovered a booking owned by another user",
			slog.String("session_id", sessionID))
		return ConfirmResult{}, errors.New("booking: confirm: ownership invariant violated")
	}
	if existing.Status == domain.StatusConfirmed {
		// Idempotent replay: this session already produced a confirmed
		// booking. Return it; the handler answers 200 instead of 201.
		return ConfirmResult{Booking: existing, Created: false}, nil
	}
	// A cancelled row occupying the session slot is unreachable in M4 (there
	// is no cancel path yet). Treat it as the conflict it structurally is.
	slog.ErrorContext(ctx, "confirm replay found a non-confirmed booking row",
		slog.String("session_id", sessionID),
		slog.String("status", string(existing.Status)))
	return ConfirmResult{}, conflict
}

// cleanupHold releases the hold after a successful commit. Best-effort on
// purpose (ADR 0006): the sale is already durable, and failing the request
// now would hide a success. If this fails, the residue is a seat key that
// blocks nothing (the phantom pre-check rejects new holds on booked seats),
// a session key whose replayed confirms still recover idempotently, and a
// ZSET bookkeeping entry that costs one hold-limit slot until TTL expiry.
// Redis's own TTL cleans all of it up; the sweeper milestone will make that
// deterministic.
func (s *Service) cleanupHold(ctx context.Context, sessionID, userID string) {
	if err := s.holds.Release(ctx, sessionID, userID); err != nil {
		slog.WarnContext(ctx, "post-confirm hold cleanup failed; residue reconciles via TTL (ADR 0006)",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
	}
}

// Release gives up a hold without confirming. The store does the ownership
// check and compare-and-delete atomically (ADR 0002) — a Get-then-Release
// here would only add a round trip and a TOCTOU gap.
func (s *Service) Release(ctx context.Context, userID, sessionID string) error {
	return s.holds.Release(ctx, sessionID, userID) // ErrHoldNotFound, ErrNotHoldOwner, infra
}

// SeatMap renders the full seat grid with per-seat status (CLAUDE.md §4).
// Precedence is booked > held > available: Postgres is the source of truth
// for what is sold, so a confirmed seat is booked even if a stale hold
// still lingers in Redis (ADR 0006's Postgres-first availability).
func (s *Service) SeatMap(ctx context.Context, screeningID string) (SeatMapView, error) {
	screening, err := s.screenings.Get(ctx, screeningID)
	if err != nil {
		return SeatMapView{}, err // ErrScreeningNotFound or infra
	}

	booked, err := s.bookings.ConfirmedSeats(ctx, screeningID)
	if err != nil {
		return SeatMapView{}, fmt.Errorf("booking: seat map: confirmed seats: %w", err)
	}
	held, err := s.holds.HeldSeats(ctx, screeningID)
	if err != nil {
		return SeatMapView{}, fmt.Errorf("booking: seat map: held seats: %w", err)
	}

	bookedSet := make(map[domain.Seat]struct{}, len(booked))
	for _, seat := range booked {
		bookedSet[seat] = struct{}{}
	}
	heldSet := make(map[domain.Seat]struct{}, len(held))
	for _, seat := range held {
		heldSet[seat] = struct{}{}
	}

	seats := make([]SeatState, 0, screening.SeatCount())
	for _, seat := range screening.AllSeats() {
		status := domain.SeatStatusAvailable
		switch {
		case hasSeat(bookedSet, seat):
			status = domain.SeatStatusBooked
		case hasSeat(heldSet, seat):
			status = domain.SeatStatusHeld
		}
		seats = append(seats, SeatState{Seat: seat, Status: status})
	}

	return SeatMapView{Screening: screening, Seats: seats}, nil
}

func hasSeat(set map[domain.Seat]struct{}, seat domain.Seat) bool {
	_, ok := set[seat]
	return ok
}
