package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	paymentadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/payment"
	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	redisadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/redis"
	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
	domainbooking "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// paymentFixture wires a PaymentRepo over the shared test database with a
// fresh priced screening and one user, mirroring newBookingFixture.
type paymentFixture struct {
	pool        *pgxpool.Pool
	repo        *postgresadapter.PaymentRepo
	screeningID string
	userID      string
}

func newPaymentFixture(t *testing.T) *paymentFixture {
	t.Helper()
	pool := connectPostgres(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	movies := postgresadapter.NewMovieRepo(pool)
	screenings := postgresadapter.NewScreeningRepo(pool)

	movie, err := movies.Create(ctx, domainmovie.Movie{
		Title: "Payments " + uuid.NewString()[:8], DurationMinutes: 90,
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
	u, err := q.CreateUser(ctx, sqlcgen.CreateUserParams{
		Email: uuid.NewString()[:8] + "@example.com", PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &paymentFixture{pool: pool, repo: postgresadapter.NewPaymentRepo(pool), screeningID: scr.ID, userID: u.ID}
}

// seedIntegrationHold places a live hold for the given seat directly in the real
// Redis hold store — the checkout precondition the payment use cases gate
// on, without running the whole booking service for it.
func seedIntegrationHold(t *testing.T, ctx context.Context, holds *redisadapter.HoldStore, screeningID, userID string, seat domainbooking.Seat) domainbooking.Hold {
	t.Helper()
	hold := domainbooking.Hold{
		SessionID: uuid.NewString(), ScreeningID: screeningID,
		Seat: seat, UserID: userID,
		HoldToken: uuid.NewString(), ExpiresAt: time.Now().Add(testHoldTTL),
	}
	if err := holds.Hold(ctx, hold); err != nil {
		t.Fatalf("seed hold: %v", err)
	}
	return hold
}

// intent builds a fresh intent row for a synthetic session.
func (f *paymentFixture) intent(session string) domain.Payment {
	return domain.Payment{
		BookingSessionID: session,
		Gateway:          "fake",
		IntentID:         "pi_fake_" + uuid.NewString(),
		AmountCents:      1450,
		Currency:         domain.DefaultCurrency,
	}
}

// TestPaymentRepoIntentInsertAndActiveLookup is the basic round-trip: the
// database assigns identity, the active lookup finds the row by session,
// and the intent lookup finds it by the gateway's ID.
func TestPaymentRepoIntentInsertAndActiveLookup(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()
	session := uuid.NewString()

	created, err := f.repo.InsertIntent(ctx, f.intent(session))
	if err != nil {
		t.Fatalf("InsertIntent: %v", err)
	}
	if created.ID == "" || created.CreatedAt.IsZero() || created.Status != domain.StatusIntent {
		t.Errorf("database must assign identity and the query must pin status: %+v", created)
	}

	bySession, err := f.repo.GetActiveBySession(ctx, session)
	if err != nil || bySession.ID != created.ID {
		t.Errorf("GetActiveBySession = %+v (err %v), want the inserted row", bySession, err)
	}
	byIntent, err := f.repo.GetByIntent(ctx, created.IntentID)
	if err != nil || byIntent.ID != created.ID {
		t.Errorf("GetByIntent = %+v (err %v), want the inserted row", byIntent, err)
	}

	if _, err := f.repo.GetActiveBySession(ctx, uuid.NewString()); !errors.Is(err, domain.ErrPaymentNotFound) {
		t.Errorf("unknown session: err = %v, want ErrPaymentNotFound", err)
	}
	if _, err := f.repo.GetByIntent(ctx, "pi_fake_nope"); !errors.Is(err, domain.ErrPaymentNotFound) {
		t.Errorf("unknown intent: err = %v, want ErrPaymentNotFound", err)
	}
}

// TestPaymentRepoActiveSessionUniquePinned asserts migration 00009's
// arbiter against the LIVE schema: a second active intent for one session
// collides on payments_active_session_unique BY NAME (a rename breaks this
// test instead of production), the repo maps it to the idempotency
// sentinel, and failing the first row frees the slot for its replacement.
func TestPaymentRepoActiveSessionUniquePinned(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()
	q := sqlcgen.New(f.pool)
	session := uuid.NewString()

	first, err := f.repo.InsertIntent(ctx, f.intent(session))
	if err != nil {
		t.Fatalf("first intent: %v", err)
	}

	// Raw insert so the CONSTRAINT is what fires, not repo logic.
	_, rawErr := q.InsertPaymentIntent(ctx, sqlcgen.InsertPaymentIntentParams{
		BookingSessionID: session, Gateway: "fake",
		IntentID: "pi_fake_" + uuid.NewString(), AmountCents: 1450, Currency: "usd",
	})
	if name := uniqueConstraintName(rawErr); name != "payments_active_session_unique" {
		t.Fatalf("second active intent: constraint = %q, want payments_active_session_unique (err=%v)", name, rawErr)
	}

	// Through the repo, the same collision is the recovery sentinel.
	if _, err := f.repo.InsertIntent(ctx, f.intent(session)); !errors.Is(err, domain.ErrPaymentIntentExists) {
		t.Errorf("repo err = %v, want ErrPaymentIntentExists", err)
	}

	// Supersede = fail-then-insert (the partial index excludes failed rows).
	if _, err := f.repo.MarkFailed(ctx, first.IntentID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	replacement, err := f.repo.InsertIntent(ctx, f.intent(session))
	if err != nil {
		t.Fatalf("superseding intent: %v", err)
	}
	active, err := f.repo.GetActiveBySession(ctx, session)
	if err != nil || active.IntentID != replacement.IntentID {
		t.Errorf("active after supersede = %+v (err %v), want the replacement", active, err)
	}
}

// TestPaymentRepoCaptureIsGuardedAndIdempotent pins the guarded-UPDATE
// semantics against real Postgres: first capture transitions, a replay is a
// no-op success, a captured row can never fail, and a failed row can never
// capture.
func TestPaymentRepoCaptureIsGuardedAndIdempotent(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()

	t.Run("capture then replay", func(t *testing.T) {
		p, err := f.repo.InsertIntent(ctx, f.intent(uuid.NewString()))
		if err != nil {
			t.Fatalf("InsertIntent: %v", err)
		}
		row, capturedNow, err := f.repo.Capture(ctx, p.IntentID)
		if err != nil || !capturedNow || row.Status != domain.StatusCaptured {
			t.Fatalf("first capture = %+v/%v (err %v), want transitioned+captured", row, capturedNow, err)
		}
		replay, replayNow, err := f.repo.Capture(ctx, p.IntentID)
		if err != nil || replayNow {
			t.Fatalf("replay = %+v/%v (err %v), want no-op success", replay, replayNow, err)
		}
		if replay.ID != row.ID || !replay.UpdatedAt.Equal(row.UpdatedAt) {
			t.Errorf("replay touched the row: %v vs %v", replay.UpdatedAt, row.UpdatedAt)
		}
		// Money that moved exits only via refund: fail is refused.
		if _, err := f.repo.MarkFailed(ctx, p.IntentID); !errors.Is(err, domain.ErrPaymentInvalidTransition) {
			t.Errorf("MarkFailed on captured: err = %v, want ErrPaymentInvalidTransition", err)
		}
	})

	t.Run("failed rows cannot capture", func(t *testing.T) {
		p, err := f.repo.InsertIntent(ctx, f.intent(uuid.NewString()))
		if err != nil {
			t.Fatalf("InsertIntent: %v", err)
		}
		if _, err := f.repo.MarkFailed(ctx, p.IntentID); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if _, _, err := f.repo.Capture(ctx, p.IntentID); !errors.Is(err, domain.ErrPaymentInvalidTransition) {
			t.Errorf("Capture on failed: err = %v, want ErrPaymentInvalidTransition", err)
		}
		// Already-failed is the silent no-op.
		again, err := f.repo.MarkFailed(ctx, p.IntentID)
		if err != nil || again.Status != domain.StatusFailed {
			t.Errorf("MarkFailed replay = %+v (err %v), want the failed row", again, err)
		}
	})

	t.Run("unknown intent", func(t *testing.T) {
		if _, _, err := f.repo.Capture(ctx, "pi_fake_ghost"); !errors.Is(err, domain.ErrPaymentNotFound) {
			t.Errorf("Capture: err = %v, want ErrPaymentNotFound", err)
		}
		if _, err := f.repo.MarkFailed(ctx, "pi_fake_ghost"); !errors.Is(err, domain.ErrPaymentNotFound) {
			t.Errorf("MarkFailed: err = %v, want ErrPaymentNotFound", err)
		}
	})
}

// TestPaymentRepoConcurrentCaptureExactlyOneTransition races 32 goroutines
// at one intent's guarded capture against real Postgres. The row lock
// serializes them: exactly one may see capturedNow=true, the rest are
// idempotent no-ops, and the row is captured exactly once. Run with -race.
func TestPaymentRepoConcurrentCaptureExactlyOneTransition(t *testing.T) {
	const contenders = 32
	f := newPaymentFixture(t)
	ctx := context.Background()

	p, err := f.repo.InsertIntent(ctx, f.intent(uuid.NewString()))
	if err != nil {
		t.Fatalf("InsertIntent: %v", err)
	}

	type outcome struct {
		capturedNow bool
		err         error
	}
	results := make([]outcome, contenders)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, now, err := f.repo.Capture(ctx, p.IntentID)
			results[i] = outcome{capturedNow: now, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	transitions := 0
	for i, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d: err = %v, want success (transition or replay)", i, r.err)
			continue
		}
		if r.capturedNow {
			transitions++
		}
	}
	if transitions != 1 {
		t.Errorf("%d goroutines saw capturedNow=true, want exactly 1", transitions)
	}
	row, err := f.repo.GetByIntent(ctx, p.IntentID)
	if err != nil || row.Status != domain.StatusCaptured {
		t.Errorf("final row = %+v (err %v), want captured", row, err)
	}
}

// TestPaymentRepoHasCapturedPayment pins the confirm gate's EXISTS read:
// closed for intent rows, open for captured ones, per session only.
func TestPaymentRepoHasCapturedPayment(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()

	session := uuid.NewString()
	other := uuid.NewString()
	p, err := f.repo.InsertIntent(ctx, f.intent(session))
	if err != nil {
		t.Fatalf("InsertIntent: %v", err)
	}
	if got, err := f.repo.HasCapturedPayment(ctx, session); err != nil || got {
		t.Errorf("gate with an uncaptured intent = %v (err %v), want false", got, err)
	}
	if _, _, err := f.repo.Capture(ctx, p.IntentID); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got, err := f.repo.HasCapturedPayment(ctx, session); err != nil || !got {
		t.Errorf("gate after capture = %v (err %v), want true", got, err)
	}
	if got, err := f.repo.HasCapturedPayment(ctx, other); err != nil || got {
		t.Errorf("gate for an unrelated session = %v (err %v), want false", got, err)
	}
}

// TestPaymentDuplicateWebhookIsNoOpAgainstRealStores replays the same
// signed capture twice through the full service + Postgres stack: the first
// applies, the duplicate acks inert, and the table ends with exactly one
// captured row — at-least-once delivery made harmless by the guarded
// UPDATE, not by application caution.
func TestPaymentDuplicateWebhookIsNoOpAgainstRealStores(t *testing.T) {
	f := newPaymentFixture(t)
	client := connectRedis(t)
	ctx := context.Background()

	screenings := postgresadapter.NewScreeningRepo(f.pool)
	holds := redisadapter.NewHoldStore(client, testHoldTTL, 10)
	gw, err := paymentadapter.NewFakeGateway(integrationWebhookSecret)
	if err != nil {
		t.Fatalf("fake gateway: %v", err)
	}
	svc := apppayment.NewService(gw, f.repo, holds, screenings, time.Now)

	// A real hold, because CreateIntent gates on it.
	hold := seedIntegrationHold(t, ctx, holds, f.screeningID, f.userID, domainbooking.Seat{Row: "A", Number: 1})

	res, err := svc.CreateIntent(ctx, f.userID, hold.SessionID)
	if err != nil {
		t.Fatalf("CreateIntent: %v", err)
	}
	body, header := gw.SignWebhook(apppayment.WebhookEvent{
		Kind: apppayment.EventCaptured, IntentID: res.Payment.IntentID, AmountCents: res.Payment.AmountCents,
	}, time.Now())

	first, err := svc.HandleWebhook(ctx, body, header)
	if err != nil || !first.Applied {
		t.Fatalf("first webhook = %+v (err %v), want applied", first, err)
	}
	dup, err := svc.HandleWebhook(ctx, body, header)
	if err != nil {
		t.Fatalf("duplicate webhook: %v", err)
	}
	if dup.Applied {
		t.Error("duplicate must ack inert (Applied=false)")
	}

	var captured int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM payments WHERE booking_session_id = $1 AND status = 'captured'`,
		hold.SessionID).Scan(&captured); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if captured != 1 {
		t.Errorf("captured rows = %d, want exactly 1", captured)
	}
}

// TestPaymentClientConfirmAgainstRealStores drives the OTHER capture path
// (client confirm, the dev fast path) through the full stack: real Redis
// hold, real Postgres payments, real sandbox gateway.
func TestPaymentClientConfirmAgainstRealStores(t *testing.T) {
	f := newPaymentFixture(t)
	client := connectRedis(t)
	ctx := context.Background()

	screenings := postgresadapter.NewScreeningRepo(f.pool)
	holds := redisadapter.NewHoldStore(client, testHoldTTL, 10)
	gw, err := paymentadapter.NewFakeGateway(integrationWebhookSecret)
	if err != nil {
		t.Fatalf("fake gateway: %v", err)
	}
	svc := apppayment.NewService(gw, f.repo, holds, screenings, time.Now)

	hold := seedIntegrationHold(t, ctx, holds, f.screeningID, f.userID, domainbooking.Seat{Row: "A", Number: 1})

	intentRes, err := svc.CreateIntent(ctx, f.userID, hold.SessionID)
	if err != nil {
		t.Fatalf("CreateIntent: %v", err)
	}
	if intentRes.Payment.AmountCents != 1450 {
		t.Errorf("frozen amount = %d, want the screening's 1450", intentRes.Payment.AmountCents)
	}

	p, err := svc.ConfirmByClient(ctx, f.userID, hold.SessionID)
	if err != nil {
		t.Fatalf("ConfirmByClient: %v", err)
	}
	if p.Status != domain.StatusCaptured {
		t.Errorf("status = %s, want captured", p.Status)
	}
	gate, err := f.repo.HasCapturedPayment(ctx, hold.SessionID)
	if err != nil || !gate {
		t.Errorf("confirm gate = %v (err %v), want open", gate, err)
	}

	// Decline path against the real store: a superseding intent after a
	// gateway refusal reuses the session slot (fail-then-insert).
	// A DIFFERENT seat: the first hold is still live (nothing on this path
	// releases it), and one seat can only be held once.
	session2 := seedIntegrationHold(t, ctx, holds, f.screeningID, f.userID, domainbooking.Seat{Row: "B", Number: 2})
	if _, err := svc.CreateIntent(ctx, f.userID, session2.SessionID); err != nil {
		t.Fatalf("second intent: %v", err)
	}
	gw.FailNextCapture(errors.New("gateway: card_declined"))
	if _, err := svc.ConfirmByClient(ctx, f.userID, session2.SessionID); !errors.Is(err, domain.ErrPaymentDeclined) {
		t.Fatalf("declined confirm: err = %v, want ErrPaymentDeclined", err)
	}
	if _, err := svc.CreateIntent(ctx, f.userID, session2.SessionID); err != nil {
		t.Errorf("superseding intent after decline: %v", err)
	}
}
