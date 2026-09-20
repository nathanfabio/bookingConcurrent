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

	// 5. Confirm: Postgres commits (booking + outbox in ONE transaction),
	// Redis is cleaned up, the map flips to booked — Postgres-first.
	res, err := svc.Confirm(ctx, user.ID, hold.SessionID)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !res.Created || res.Booking.Seat != seatA1 {
		t.Errorf("confirm result = %+v", res)
	}
	status = seatStatus(t, svc, ctx, scr.ID, seatA1)
	if status != domain.SeatStatusBooked {
		t.Errorf("seat A1 after confirm = %q, want booked", status)
	}

	// The BookingConfirmed outbox row landed with the booking (the relay
	// that publishes it is M6; until then it must sit pending).
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

	// 7. Replay of the confirm is idempotent only while the hold survives;
	// after the cleanup released it, the session is simply unknown — the
	// same generic 404 the domain promises.
	if _, err := svc.Confirm(ctx, user.ID, hold.SessionID); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("confirm after cleanup: err = %v, want ErrHoldNotFound", err)
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

	// Short-TTL service: 1s holds, mirroring the hold-store lifecycle test.
	shortHolds := redisadapter.NewHoldStore(client, 1*time.Second, 10)
	payments := postgresadapter.NewPaymentRepo(pool)
	svc := appbooking.NewService(shortHolds, bookings, screenings, payments, 1*time.Second, time.Now)

	seat := domain.Seat{Row: "A", Number: 1}
	hold, err := svc.Hold(ctx, user.ID, scr.ID, seat)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	// Capture the money BEFORE the hold dies: ADR 0006's hard NO must hold
	// even with real captured funds (ADR 0008) — an expired hold is not
	// sellable, and the payment row is the audit trail for the refund ops.
	inserted, err := payments.InsertIntent(ctx, domainpayment.Payment{
		BookingSessionID: hold.SessionID, Gateway: "fake",
		IntentID: "pi_fake_" + uuid.NewString(), AmountCents: 1500,
		Currency: domainpayment.DefaultCurrency,
	})
	if err != nil {
		t.Fatalf("insert intent: %v", err)
	}
	if _, capturedNow, err := payments.Capture(ctx, inserted.IntentID); err != nil || !capturedNow {
		t.Fatalf("capture: capturedNow=%v err=%v", capturedNow, err)
	}

	time.Sleep(2200 * time.Millisecond) // TTL 1s + scheduler margin

	if _, err := svc.Confirm(ctx, user.ID, hold.SessionID); !errors.Is(err, domain.ErrHoldNotFound) && !errors.Is(err, domain.ErrHoldExpired) {
		t.Errorf("confirm after expiry WITH captured payment: err = %v, want ErrHoldNotFound/ErrHoldExpired", err)
	}

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
