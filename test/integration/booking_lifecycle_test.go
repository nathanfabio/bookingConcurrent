package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	paymentadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/payment"
	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	redisadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/redis"
	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
	domainpayment "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationWebhookSecret is a test fixture, not a credential: it only
// signs webhooks these tests forge against the in-process fake gateway.
const integrationWebhookSecret = "integration-fake-webhook-secret-000"

// TestBookingLifecycle drives the FULL application service against real
// Redis and real Postgres: hold → seat map held → payment intent → signed
// webhook capture → confirm → seat map booked → phantom defense → release.
// This is the end-to-end proof that the stores cooperate under ADR 0006's
// consistency model and ADR 0008's capture gate — the capture arrives as a
// REAL signed webhook through the production verification code path, the
// way a provider would deliver it.
func TestBookingLifecycle(t *testing.T) {
	pool := connectPostgres(t)
	client := connectRedis(t)
	ctx := context.Background()

	movies := postgresadapter.NewMovieRepo(pool)
	screenings := postgresadapter.NewScreeningRepo(pool)
	bookings := postgresadapter.NewBookingRepo(pool)
	payments := postgresadapter.NewPaymentRepo(pool)
	holds := redisadapter.NewHoldStore(client, testHoldTTL, 10)
	svc := appbooking.NewService(holds, bookings, screenings, payments, testHoldTTL, time.Now)

	gw, err := paymentadapter.NewFakeGateway(integrationWebhookSecret)
	if err != nil {
		t.Fatalf("fake gateway: %v", err)
	}
	paySvc := apppayment.NewService(gw, payments, holds, screenings, time.Now)

	movie, err := movies.Create(ctx, domainmovie.Movie{
		Title: "Lifecycle " + uuid.NewString()[:8], DurationMinutes: 100,
	})
	if err != nil {
		t.Fatalf("create movie: %v", err)
	}
	scr, err := screenings.Create(ctx, domainmovie.Screening{
		MovieID: movie.ID, StartsAt: time.Now().Add(time.Hour),
		Rows: []string{"A", "B"}, SeatsPerRow: 3, PriceCents: 1450,
	})
	if err != nil {
		t.Fatalf("create screening: %v", err)
	}
	user, err := sqlcgen.New(pool).CreateUser(ctx, sqlcgen.CreateUserParams{
		Email: uuid.NewString()[:8] + "@example.com", PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seatA1 := domain.Seat{Row: "A", Number: 1}

	// 1. Hold: the seat map flips A1 to held.
	hold, err := svc.Hold(ctx, user.ID, scr.ID, seatA1)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	status := seatStatus(t, svc, ctx, scr.ID, seatA1)
	if status != domain.SeatStatusHeld {
		t.Errorf("seat A1 after hold = %q, want held", status)
	}

	// 2. Confirm BEFORE paying is refused at the gate (ADR 0008): no
	// captured money, no booking — and the hold survives untouched.
	if _, err := svc.Confirm(ctx, user.ID, hold.SessionID); !errors.Is(err, domainpayment.ErrPaymentNotCaptured) {
		t.Fatalf("unpaid confirm: err = %v, want ErrPaymentNotCaptured", err)
	}

	// 3. Payment intent: the screening's price freezes into the row.
	intentRes, err := paySvc.CreateIntent(ctx, user.ID, hold.SessionID)
	if err != nil {
		t.Fatalf("CreateIntent: %v", err)
	}
	if intentRes.Payment.AmountCents != 1450 || intentRes.Payment.Status != domainpayment.StatusIntent {
		t.Fatalf("intent = %+v, want 1450/intent", intentRes.Payment)
	}

	// 4. Capture arrives as a SIGNED WEBHOOK, verified by the production
	// code path and applied through the guarded UPDATE.
	body, header := gw.SignWebhook(apppayment.WebhookEvent{
		Kind: apppayment.EventCaptured, IntentID: intentRes.Payment.IntentID, AmountCents: 1450,
	}, time.Now())
	outcome, err := paySvc.HandleWebhook(ctx, body, header)
	if err != nil || !outcome.Applied {
		t.Fatalf("webhook: outcome = %+v, err = %v, want applied", outcome, err)
	}

	assertReviewState(t, pool, intentRes.Payment.ID, "before_deadline")

	// 5. Confirm: Postgres commits (booking + outbox in ONE transaction),
	// Redis is cleaned up, the map flips to booked — Postgres-first.
	res, err := svc.Confirm(ctx, user.ID, hold.SessionID)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !res.Created || res.Booking.Seat != seatA1 {
		t.Errorf("confirm result = %+v", res)
	}
	assertReviewState(t, pool, intentRes.Payment.ID, "")
	status = seatStatus(t, svc, ctx, scr.ID, seatA1)
	if status != domain.SeatStatusBooked {
		t.Errorf("seat A1 after confirm = %q, want booked", status)
	}

	// The BookingConfirmed outbox row landed with the booking. It must sit
	// PENDING here: the relay lives in cmd/worker, which this test does not
	// run — publishing is outbox_relay_test.go's half of the contract, and
	// this assertion pins the writing half (ADR 0006's commit point).
	var pending int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE event_type = 'BookingConfirmed' AND payload->>'session_id' = $1 AND published_at IS NULL`,
		hold.SessionID).Scan(&pending); err != nil {
		t.Fatalf("outbox query: %v", err)
	}
	if pending != 1 {
		t.Errorf("pending BookingConfirmed events = %d, want exactly 1", pending)
	}

	// 6. Phantom-hold defense end-to-end: the seat is sold, so a new hold
	// is rejected before touching Redis.
	if _, err := svc.Hold(ctx, user.ID, scr.ID, seatA1); !errors.Is(err, domain.ErrSeatAlreadyBooked) {
		t.Errorf("hold on booked seat: err = %v, want ErrSeatAlreadyBooked", err)
	}

	// 7. Replay reads the durable result even after Redis cleanup.
	replay, err := svc.Confirm(ctx, user.ID, hold.SessionID)
	if err != nil || replay.Created || replay.Booking.ID != res.Booking.ID {
		t.Errorf("confirm after cleanup: %+v, %v; want original booking", replay, err)
	}

	// 8. Release on a fresh hold frees the seat again.
	hold2, err := svc.Hold(ctx, user.ID, scr.ID, domain.Seat{Row: "B", Number: 2})
	if err != nil {
		t.Fatalf("Hold B2: %v", err)
	}
	if err := svc.Release(ctx, user.ID, hold2.SessionID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	status = seatStatus(t, svc, ctx, scr.ID, domain.Seat{Row: "B", Number: 2})
	if status != domain.SeatStatusAvailable {
		t.Errorf("seat B2 after release = %q, want available", status)
	}
}

// TestBookingLifecycleExpiry proves the lifecycle against real TTL decay:
// an expired hold cannot be confirmed, and its seat becomes claimable
// again — with NO sweeper involved. Redis's own TTL does the work; the
// sweeper milestone will only make the bookkeeping cleanup deterministic
// (ADR 0006).
func TestBookingLifecycleExpiry(t *testing.T) {
	pool := connectPostgres(t)
	client := connectRedis(t)
	ctx := context.Background()

	movies := postgresadapter.NewMovieRepo(pool)
	screenings := postgresadapter.NewScreeningRepo(pool)
	bookings := postgresadapter.NewBookingRepo(pool)

	movie, err := movies.Create(ctx, domainmovie.Movie{
		Title: "Expiry " + uuid.NewString()[:8], DurationMinutes: 100,
	})
	if err != nil {
		t.Fatalf("create movie: %v", err)
	}
	scr, err := screenings.Create(ctx, domainmovie.Screening{
		MovieID: movie.ID, StartsAt: time.Now().Add(time.Hour),
		Rows: []string{"A"}, SeatsPerRow: 2, PriceCents: 1500,
	})
	if err != nil {
		t.Fatalf("create screening: %v", err)
	}
	user, err := sqlcgen.New(pool).CreateUser(ctx, sqlcgen.CreateUserParams{
		Email: uuid.NewString()[:8] + "@example.com", PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Allow time for intent creation and capture before the real Redis TTL elapses.
	const ttl = 3 * time.Second
	shortHolds := redisadapter.NewHoldStore(client, ttl, 10)
	payments := postgresadapter.NewPaymentRepo(pool)
	svc := appbooking.NewService(shortHolds, bookings, screenings, payments, ttl, time.Now)

	seat := domain.Seat{Row: "A", Number: 1}
	hold, err := svc.Hold(ctx, user.ID, scr.ID, seat)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	// Capture the money BEFORE the hold dies: ADR 0006's hard NO must hold
	// even with real captured funds (ADR 0008) — an expired hold is not
	// sellable, and the payment row is the audit trail for the refund ops.
	gw, err := paymentadapter.NewFakeGateway(integrationWebhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	checkout := apppayment.NewService(gw, payments, shortHolds, screenings, time.Now)
	intent, err := checkout.CreateIntent(ctx, user.ID, hold.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	inserted := intent.Payment
	assertReviewState(t, pool, inserted.ID, "")
	if _, capturedNow, err := payments.Capture(ctx, inserted.IntentID); err != nil || !capturedNow {
		t.Fatalf("capture: capturedNow=%v err=%v", capturedNow, err)
	}

	time.Sleep(time.Until(hold.ExpiresAt.Add(200 * time.Millisecond)))

	if _, err := svc.Confirm(ctx, user.ID, hold.SessionID); !errors.Is(err, domain.ErrHoldNotFound) && !errors.Is(err, domain.ErrHoldExpired) {
		t.Errorf("confirm after expiry WITH captured payment: err = %v, want ErrHoldNotFound/ErrHoldExpired", err)
	}

	assertReviewState(t, pool, inserted.ID, "deadline_passed")

	// The seat is claimable again — expiry alone reconciled it.
	if _, err := svc.Hold(ctx, user.ID, scr.ID, seat); err != nil {
		t.Errorf("re-hold after expiry: %v", err)
	}
}

func seatStatus(t *testing.T, svc *appbooking.Service, ctx context.Context, screeningID string, seat domain.Seat) domain.SeatStatus {
	t.Helper()
	view, err := svc.SeatMap(ctx, screeningID)
	if err != nil {
		t.Fatalf("SeatMap: %v", err)
	}
	for _, st := range view.Seats {
		if st.Seat == seat {
			return st.Status
		}
	}
	t.Fatalf("seat %s%d missing from seat map", seat.Row, seat.Number)
	return ""
}

// An empty expected state means the payment should be absent from the report.
func assertReviewState(t *testing.T, pool *pgxpool.Pool, paymentID, want string) {
	t.Helper()
	var state string
	err := pool.QueryRow(t.Context(), "SELECT review_state FROM unbooked_captures WHERE payment_id = $1", paymentID).Scan(&state)
	if want == "" && errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil || state != want {
		t.Fatalf("payment %s review state = %q, err=%v, want %q", paymentID, state, err, want)
	}
}
