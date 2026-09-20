// External test package: these tests drive the Service through the
// in-memory fakes AND the real fake gateway (adapters/payment) — the
// sandbox is deterministic and offline, so exercising its actual
// sign/verify code is both safe and the point (§9). Test files are exempt
// from the application-purity depguard rule for exactly this reason.
package payment_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/memory"
	paymentadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/payment"
	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
	domainbooking "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
)

var epoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

const (
	testHoldTTL    = 5 * time.Minute
	testScreening  = "screening-1"
	testPriceCents = 1450
	testSecret     = "service-test-webhook-secret"
	alice          = "user-alice"
	bob            = "user-bob"
	sessionID      = "session-1"
	testHoldToken  = "hold-token-1"
)

type testEnv struct {
	clock      *memory.ManualClock
	holds      *memory.HoldStore
	screenings *memory.ScreeningStore
	payments   *memory.PaymentStore
	gw         *paymentadapter.FakeGateway
	svc        *apppayment.Service
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	clock := memory.NewManualClock(epoch)
	gw, err := paymentadapter.NewFakeGateway(testSecret)
	if err != nil {
		t.Fatalf("fake gateway: %v", err)
	}
	env := &testEnv{
		clock:      clock,
		holds:      memory.NewHoldStore(clock, 4),
		screenings: memory.NewScreeningStore(),
		payments:   memory.NewPaymentStore(clock),
		gw:         gw,
	}
	env.screenings.Add(domainmovie.Screening{
		ID: testScreening, MovieID: "movie-1",
		StartsAt: epoch.Add(24 * time.Hour),
		Rows:     []string{"A", "B"}, SeatsPerRow: 3,
		PriceCents: testPriceCents,
	})
	env.svc = apppayment.NewService(gw, env.payments, env.holds, env.screenings, clock.Now)
	return env
}

// seedHold places a live hold directly in the fake store — the checkout
// precondition every client-facing payment use case gates on.
func (env *testEnv) seedHold(t *testing.T, session, userID string) domainbooking.Hold {
	t.Helper()
	hold := domainbooking.Hold{
		SessionID: session, ScreeningID: testScreening,
		Seat: domainbooking.Seat{Row: "A", Number: 1}, UserID: userID,
		HoldToken: testHoldToken, ExpiresAt: epoch.Add(testHoldTTL),
	}
	if err := env.holds.Hold(context.Background(), hold); err != nil {
		t.Fatalf("seed hold: %v", err)
	}
	return hold
}

// signedCapture renders a provider-shaped capture webhook for an intent the
// env already minted, signed with the REAL scheme at the given time.
func (env *testEnv) signedCapture(intentID string, amountCents int, at time.Time) ([]byte, string) {
	return env.gw.SignWebhook(apppayment.WebhookEvent{
		Kind: apppayment.EventCaptured, IntentID: intentID, AmountCents: amountCents,
	}, at)
}

func TestServiceCreateIntent(t *testing.T) {
	ctx := context.Background()

	t.Run("freezes the screening price into a new intent", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)

		res, err := env.svc.CreateIntent(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("CreateIntent: %v", err)
		}
		if !res.Created {
			t.Error("first call must report Created=true")
		}
		p := res.Payment
		if p.ID == "" || p.BookingSessionID != sessionID || p.Gateway != "fake" {
			t.Errorf("payment = %+v", p)
		}
		if p.Status != domain.StatusIntent {
			t.Errorf("status = %s, want intent", p.Status)
		}
		// The amount is the screening's price, never recomputed later.
		if p.AmountCents != testPriceCents || p.Currency != domain.DefaultCurrency {
			t.Errorf("amount = %d %s, want %d usd", p.AmountCents, p.Currency, testPriceCents)
		}
		if !res.HoldExpiresAt.Equal(epoch.Add(testHoldTTL)) {
			t.Errorf("HoldExpiresAt = %v, want the hold's deadline", res.HoldExpiresAt)
		}
	})

	t.Run("replay returns the same intent", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)

		first, err := env.svc.CreateIntent(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		replay, err := env.svc.CreateIntent(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if replay.Created {
			t.Error("replay must report Created=false")
		}
		if replay.Payment.IntentID != first.Payment.IntentID || replay.Payment.ID != first.Payment.ID {
			t.Errorf("replay minted a second intent: %s vs %s",
				replay.Payment.IntentID, first.Payment.IntentID)
		}
	})

	t.Run("hold gate rejects unknown, foreign, and expired sessions", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)

		if _, err := env.svc.CreateIntent(ctx, alice, "no-such-session"); !errors.Is(err, domainbooking.ErrHoldNotFound) {
			t.Errorf("unknown: err = %v, want ErrHoldNotFound", err)
		}
		if _, err := env.svc.CreateIntent(ctx, bob, sessionID); !errors.Is(err, domainbooking.ErrNotHoldOwner) {
			t.Errorf("foreign: err = %v, want ErrNotHoldOwner", err)
		}
		env.clock.Advance(testHoldTTL)
		// At the inclusive boundary the fake stops SERVING the hold (mirroring
		// Redis's TTL eviction), so the gate surfaces the store's sentinel;
		// the service's own ErrHoldExpired branch fires only in the
		// sub-second window the servedPastExpiry case models (see the
		// booking service tests). Either way: rejected, nothing recorded.
		if _, err := env.svc.CreateIntent(ctx, alice, sessionID); !errors.Is(err, domainbooking.ErrHoldNotFound) {
			t.Errorf("expired: err = %v, want ErrHoldNotFound", err)
		}
		// Nothing was ever recorded: the gate runs before gateway and store.
		if _, err := env.payments.GetActiveBySession(ctx, sessionID); !errors.Is(err, domain.ErrPaymentNotFound) {
			t.Errorf("payments after gate rejections: err = %v, want ErrPaymentNotFound", err)
		}
	})

	t.Run("gateway failure surfaces wrapped and records nothing", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		env.gw.FailNextCreate(errors.New("gateway: outage"))

		_, err := env.svc.CreateIntent(ctx, alice, sessionID)
		if err == nil || !strings.Contains(err.Error(), "gateway") {
			t.Fatalf("err = %v, want a wrapped gateway failure", err)
		}
		if _, getErr := env.payments.GetActiveBySession(ctx, sessionID); !errors.Is(getErr, domain.ErrPaymentNotFound) {
			t.Errorf("a failed gateway call must not record a payment: %v", getErr)
		}
	})

	t.Run("a declined intent can be superseded", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)

		first, err := env.svc.CreateIntent(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("first intent: %v", err)
		}
		env.gw.FailNextCapture(errors.New("gateway: card_declined"))
		if _, err := env.svc.ConfirmByClient(ctx, alice, sessionID); !errors.Is(err, domain.ErrPaymentDeclined) {
			t.Fatalf("declined confirm: err = %v, want ErrPaymentDeclined", err)
		}

		// The decline FAILED the row, freeing the active-intent slot: a new
		// checkout mints a fresh intent (migration 00009's supersede path).
		second, err := env.svc.CreateIntent(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("superseding intent: %v", err)
		}
		if !second.Created || second.Payment.IntentID == first.Payment.IntentID {
			t.Errorf("supersede = %+v (created %v), want a fresh intent", second.Payment, second.Created)
		}
	})

	t.Run("insert race recovers the winner", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)

		// The winner's row exists but the service's reuse lookup misses it
		// (committed a microsecond too late), so the insert hits the arbiter
		// and recovery must return the winner — the booking-confirm recovery
		// shape applied to payments (ADR 0008).
		winner, err := env.payments.InsertIntent(ctx, domain.Payment{
			BookingSessionID: sessionID, Gateway: "fake",
			IntentID: "pi_fake_winner", AmountCents: testPriceCents, Currency: domain.DefaultCurrency,
		})
		if err != nil {
			t.Fatalf("seed winner: %v", err)
		}
		raced := &racedInsertStore{PaymentStore: env.payments}
		svc := apppayment.NewService(env.gw, raced, env.holds, env.screenings, env.clock.Now)

		res, err := svc.CreateIntent(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("CreateIntent through the raced store: %v", err)
		}
		if res.Created {
			t.Error("the racer must not report Created=true")
		}
		if res.Payment.IntentID != winner.IntentID {
			t.Errorf("recovery returned %s, want the winner %s", res.Payment.IntentID, winner.IntentID)
		}
		if !raced.conflictWasServed() {
			t.Error("test setup bug: the insert-race branch was never exercised")
		}
	})
}

// racedInsertStore simulates losing the CreateIntent insert race: the first
// active lookup misses (the rival was not yet visible), the insert then
// collides with the arbiter exactly once, and later reads see the winner.
type racedInsertStore struct {
	*memory.PaymentStore
	mu             sync.Mutex
	missedLookup   bool
	conflictServed bool
}

func (s *racedInsertStore) GetActiveBySession(ctx context.Context, sessionID string) (domain.Payment, error) {
	s.mu.Lock()
	first := !s.missedLookup
	s.missedLookup = true
	s.mu.Unlock()
	if first {
		return domain.Payment{}, domain.ErrPaymentNotFound
	}
	return s.PaymentStore.GetActiveBySession(ctx, sessionID)
}

func (s *racedInsertStore) InsertIntent(ctx context.Context, p domain.Payment) (domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conflictServed = true
	return domain.Payment{}, domain.ErrPaymentIntentExists
}

func (s *racedInsertStore) conflictWasServed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conflictServed
}

func TestServiceConfirmByClient(t *testing.T) {
	ctx := context.Background()

	mustIntent := func(t *testing.T, env *testEnv) string {
		t.Helper()
		res, err := env.svc.CreateIntent(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("setup intent: %v", err)
		}
		return res.Payment.IntentID
	}

	t.Run("capture flips the row and is replay-safe", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		intent := mustIntent(t, env)

		p, err := env.svc.ConfirmByClient(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("ConfirmByClient: %v", err)
		}
		if p.Status != domain.StatusCaptured || p.IntentID != intent || p.AmountCents != testPriceCents {
			t.Errorf("payment = %+v, want captured %s for %d", p, intent, testPriceCents)
		}

		replay, err := env.svc.ConfirmByClient(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if replay.ID != p.ID || replay.Status != domain.StatusCaptured {
			t.Errorf("replay = %+v, want the same captured row", replay)
		}
		captured, err := env.payments.HasCapturedPayment(ctx, sessionID)
		if err != nil || !captured {
			t.Errorf("confirm gate read = %v (err %v), want true", captured, err)
		}
	})

	t.Run("without an intent the answer is payment-not-captured", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)

		if _, err := env.svc.ConfirmByClient(ctx, alice, sessionID); !errors.Is(err, domain.ErrPaymentNotCaptured) {
			t.Errorf("err = %v, want ErrPaymentNotCaptured", err)
		}
	})

	t.Run("an expired hold is never charged", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		mustIntent(t, env)
		counting := &countingGateway{PaymentGateway: env.gw}
		svc := apppayment.NewService(counting, env.payments, env.holds, env.screenings, env.clock.Now)

		env.clock.Advance(testHoldTTL) // inclusive expiry: the fake stops serving
		if _, err := svc.ConfirmByClient(ctx, alice, sessionID); !errors.Is(err, domainbooking.ErrHoldNotFound) {
			t.Fatalf("err = %v, want ErrHoldNotFound (gate rejected the expired hold)", err)
		}
		if n := counting.captures(); n != 0 {
			t.Errorf("gateway CaptureIntent called %d times — the hold gate must run BEFORE any charge", n)
		}
	})

	t.Run("a decline marks the row failed and says so", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		intent := mustIntent(t, env)
		env.gw.FailNextCapture(errors.New("gateway: card_declined"))

		_, err := env.svc.ConfirmByClient(ctx, alice, sessionID)
		if !errors.Is(err, domain.ErrPaymentDeclined) {
			t.Fatalf("err = %v, want ErrPaymentDeclined", err)
		}
		p, getErr := env.payments.GetByIntent(ctx, intent)
		if getErr != nil {
			t.Fatalf("GetByIntent: %v", getErr)
		}
		if p.Status != domain.StatusFailed {
			t.Errorf("status = %s, want failed (the decline must be recorded)", p.Status)
		}
	})
}

// countingGateway decorates the sandbox to count capture calls — the only
// way to prove "never charge an expired hold" is to watch the gateway, not
// the store.
type countingGateway struct {
	apppayment.PaymentGateway
	mu       sync.Mutex
	captureN int
}

func (g *countingGateway) CaptureIntent(ctx context.Context, intentID string, amountCents int) error {
	g.mu.Lock()
	g.captureN++
	g.mu.Unlock()
	return g.PaymentGateway.CaptureIntent(ctx, intentID, amountCents)
}

func (g *countingGateway) captures() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.captureN
}

func TestServiceHandleWebhook(t *testing.T) {
	ctx := context.Background()

	mustIntent := func(t *testing.T, env *testEnv) string {
		t.Helper()
		res, err := env.svc.CreateIntent(ctx, alice, sessionID)
		if err != nil {
			t.Fatalf("setup intent: %v", err)
		}
		return res.Payment.IntentID
	}

	t.Run("a signed capture applies exactly once", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		intent := mustIntent(t, env)

		body, header := env.signedCapture(intent, testPriceCents, time.Now())
		outcome, err := env.svc.HandleWebhook(ctx, body, header)
		if err != nil {
			t.Fatalf("HandleWebhook: %v", err)
		}
		if !outcome.Applied || outcome.IntentID != intent {
			t.Errorf("outcome = %+v, want applied for %s", outcome, intent)
		}
		p, err := env.payments.GetByIntent(ctx, intent)
		if err != nil || p.Status != domain.StatusCaptured {
			t.Fatalf("row = %+v (err %v), want captured", p, err)
		}

		// At-least-once delivery: the duplicate is a NO-OP ack, not an
		// error and not a second application.
		dup, err := env.svc.HandleWebhook(ctx, body, header)
		if err != nil {
			t.Fatalf("duplicate: %v", err)
		}
		if dup.Applied {
			t.Error("duplicate webhook must report Applied=false")
		}
	})

	t.Run("bad signatures are rejected opaquely", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		intent := mustIntent(t, env)

		body, header := env.signedCapture(intent, testPriceCents, time.Now())
		otherGW, err := paymentadapter.NewFakeGateway("a-different-secret")
		if err != nil {
			t.Fatal(err)
		}
		forgedBody, forgedHeader := otherGW.SignWebhook(apppayment.WebhookEvent{
			Kind: apppayment.EventCaptured, IntentID: intent, AmountCents: testPriceCents,
		}, time.Now())
		staleBody, staleHeader := env.signedCapture(intent, testPriceCents, time.Now().Add(-time.Hour))

		cases := map[string]struct {
			body   []byte
			header string
		}{
			"missing header":  {body, ""},
			"malformed":       {body, "nonsense"},
			"tampered body":   {append(append([]byte{}, body...), 'X'), header},
			"wrong secret":    {forgedBody, forgedHeader},
			"stale timestamp": {staleBody, staleHeader},
		}
		for name, tc := range cases {
			_, err := env.svc.HandleWebhook(ctx, tc.body, tc.header)
			if !errors.Is(err, domain.ErrWebhookInvalid) {
				t.Errorf("%s: err = %v, want ErrWebhookInvalid", name, err)
			}
		}
		// Nothing moved despite the forged capture attempts.
		if captured, err := env.payments.HasCapturedPayment(ctx, sessionID); err != nil || captured {
			t.Errorf("captured = %v (err %v), want false", captured, err)
		}
	})

	t.Run("an unknown intent is acked without applying", func(t *testing.T) {
		env := newTestEnv(t)
		body, header := env.signedCapture("pi_fake_never-minted", testPriceCents, time.Now())

		outcome, err := env.svc.HandleWebhook(ctx, body, header)
		if err != nil {
			t.Fatalf("ack policy: err = %v, want nil (retrying cannot help)", err)
		}
		if outcome.Applied {
			t.Error("unknown intent must report Applied=false")
		}
	})

	t.Run("an amount mismatch fails the payment", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		intent := mustIntent(t, env)

		// The provider claims it moved the wrong amount: do not capture,
		// fail the row, ack so the retry storm stops (ADR 0008).
		body, header := env.signedCapture(intent, testPriceCents+1, time.Now())
		outcome, err := env.svc.HandleWebhook(ctx, body, header)
		if err != nil {
			t.Fatalf("HandleWebhook: %v", err)
		}
		if outcome.Applied {
			t.Error("mismatched amount must not apply")
		}
		p, err := env.payments.GetByIntent(ctx, intent)
		if err != nil || p.Status != domain.StatusFailed {
			t.Errorf("row = %+v (err %v), want failed", p, err)
		}
		if captured, _ := env.payments.HasCapturedPayment(ctx, sessionID); captured {
			t.Error("the confirm gate must stay closed on a mismatch")
		}
	})

	t.Run("a failed event marks the row failed", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		intent := mustIntent(t, env)

		body, header := env.gw.SignWebhook(apppayment.WebhookEvent{
			Kind: apppayment.EventFailed, IntentID: intent, AmountCents: testPriceCents,
		}, time.Now())
		outcome, err := env.svc.HandleWebhook(ctx, body, header)
		if err != nil {
			t.Fatalf("HandleWebhook: %v", err)
		}
		if !outcome.Applied {
			t.Error("failed event must apply")
		}
		p, err := env.payments.GetByIntent(ctx, intent)
		if err != nil || p.Status != domain.StatusFailed {
			t.Errorf("row = %+v (err %v), want failed", p, err)
		}
		// With the intent dead, the client-confirm path reports "no captured
		// payment" — the slot is free for a superseding intent.
		if _, err := env.svc.ConfirmByClient(ctx, alice, sessionID); !errors.Is(err, domain.ErrPaymentNotCaptured) {
			t.Errorf("confirm after failed event: err = %v, want ErrPaymentNotCaptured", err)
		}
	})

	t.Run("an unhandled event kind is a forward-compatible ack", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		intent := mustIntent(t, env)

		body, header := env.gw.SignWebhook(apppayment.WebhookEvent{
			Kind: apppayment.EventKind("payment.dispute_opened"), IntentID: intent, AmountCents: testPriceCents,
		}, time.Now())
		outcome, err := env.svc.HandleWebhook(ctx, body, header)
		if err != nil {
			t.Fatalf("HandleWebhook: %v", err)
		}
		if outcome.Applied {
			t.Error("unhandled kind must not apply")
		}
	})

	t.Run("capture after hold expiry records provider truth", func(t *testing.T) {
		env := newTestEnv(t)
		env.seedHold(t, sessionID, alice)
		intent := mustIntent(t, env)

		// The webhook path never consults the hold: money that moved is
		// recorded even though the seat is no longer confirmable (the
		// booking side pins the hard NO; ADR 0006 + ADR 0008).
		env.clock.Advance(testHoldTTL)
		body, header := env.signedCapture(intent, testPriceCents, time.Now())
		outcome, err := env.svc.HandleWebhook(ctx, body, header)
		if err != nil || !outcome.Applied {
			t.Fatalf("outcome = %+v, err = %v, want applied", outcome, err)
		}
		if captured, _ := env.payments.HasCapturedPayment(ctx, sessionID); !captured {
			t.Error("the captured row is the audit trail — it must exist")
		}
	})
}

// TestServiceConcurrentWebhookAndClientConfirmOneCapture is the §9
// concurrency proof for the capture path: a signed webhook and many client
// confirms race on the same intent. Every path must succeed (no error),
// the webhook may or may not be the one that applied, and the row ends
// captured exactly once — under -race, which is the real assertion here
// (the fakes' mutexes must make the interleavings safe).
func TestServiceConcurrentWebhookAndClientConfirmOneCapture(t *testing.T) {
	ctx := context.Background()
	const contenders = 16

	env := newTestEnv(t)
	env.seedHold(t, sessionID, alice)
	res, err := env.svc.CreateIntent(ctx, alice, sessionID)
	if err != nil {
		t.Fatalf("setup intent: %v", err)
	}
	body, header := env.signedCapture(res.Payment.IntentID, testPriceCents, time.Now())

	var wg sync.WaitGroup
	errs := make([]error, contenders+1)
	applied := make([]bool, contenders+1)
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		outcome, err := env.svc.HandleWebhook(ctx, body, header)
		errs[0] = err
		applied[0] = outcome.Applied
	}()
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			p, err := env.svc.ConfirmByClient(ctx, alice, sessionID)
			errs[i+1] = err
			applied[i+1] = err == nil && p.Status == domain.StatusCaptured
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d: err = %v, want success on every path", i, err)
		}
	}
	seen := 0
	for _, ok := range applied {
		if ok {
			seen++
		}
	}
	if seen < contenders {
		t.Errorf("%d client racers saw the captured row, want all %d", seen, contenders)
	}
	captured, err := env.payments.HasCapturedPayment(ctx, sessionID)
	if err != nil || !captured {
		t.Errorf("final gate read = %v (err %v), want true", captured, err)
	}
	// The row is captured exactly once: one row, one status, no duplicates
	// (the active-session arbiter made a second intent impossible).
	p, err := env.payments.GetByIntent(ctx, res.Payment.IntentID)
	if err != nil || p.Status != domain.StatusCaptured || p.AmountCents != testPriceCents {
		t.Errorf("final row = %+v (err %v), want captured %d", p, err, testPriceCents)
	}
}
