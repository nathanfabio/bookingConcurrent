package integration

import (
	"context"
	"sync"
	"testing"

	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"

	"github.com/google/uuid"
)

// TestProcessedEventClaimConcurrentExactlyOneWinner pins the consumer's
// idempotency foundation against the REAL arbiter: 32 goroutines race to
// claim one event_id through ProcessedEventRepo, and the primary key's
// INSERT .. ON CONFLICT DO NOTHING must admit exactly one winner — the same
// schema-as-arbiter posture the booking seat index (TestConcurrentConfirm-
// ExactlyOneWinner) and the payment capture guard (TestPaymentRepoConcurrent-
// CaptureExactlyOneTransition) are proven by. The in-memory fake's mutex
// models this for unit tests; this test proves Postgres actually does it.
func TestProcessedEventClaimConcurrentExactlyOneWinner(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	repo := postgresadapter.NewProcessedEventRepo(pool)

	eventID := uuid.NewString()

	const goroutines = 32
	var wg sync.WaitGroup
	results := make([]bool, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})

	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			<-start // maximize the collision window
			results[i], errs[i] = repo.Claim(ctx, eventID, appnotification.EventTypeBookingConfirmed)
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for i := range goroutines {
		if errs[i] != nil {
			t.Errorf("goroutine %d: Claim error: %v", i, errs[i])
		}
		if results[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("claim winners = %d, want exactly 1 under %d racing claims", winners, goroutines)
	}

	// Exactly one row, stamped with the event type.
	var count int
	var eventType string
	if err := pool.QueryRow(ctx,
		"SELECT count(*), min(event_type) FROM processed_events WHERE event_id = $1", eventID).
		Scan(&count, &eventType); err != nil {
		t.Fatalf("inspect processed_events: %v", err)
	}
	if count != 1 {
		t.Errorf("processed_events rows = %d, want 1", count)
	}
	if eventType != appnotification.EventTypeBookingConfirmed {
		t.Errorf("event_type = %q, want %q", eventType, appnotification.EventTypeBookingConfirmed)
	}

	// A later sequential claim is a duplicate, not an error.
	claimed, err := repo.Claim(ctx, eventID, appnotification.EventTypeBookingConfirmed)
	if err != nil {
		t.Fatalf("sequential re-claim: %v", err)
	}
	if claimed {
		t.Error("re-claim of a processed event returned true, want false (duplicate)")
	}
}
