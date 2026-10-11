// External test package: these tests drive the Sweeper through the
// in-memory hold fake (adapters/memory), whose lazy-purge behavior models
// Redis's silent TTL expiry (ADR 0002/0010). Test files are exempt from the
// application-purity depguard rule for exactly this reason (CLAUDE.md §9).
package holdexpiry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nathanfabio/bookingConcurrent/internal/adapters/memory"
	appholdexpiry "github.com/nathanfabio/bookingConcurrent/internal/application/holdexpiry"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

var epoch = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

const holdTTL = 5 * time.Minute

func newHold(userID, screeningID, row string, num int, expires time.Time) domain.Hold {
	seat, err := domain.NewSeat(row, num)
	if err != nil {
		panic(err)
	}
	return domain.Hold{
		SessionID:   uuid.NewString(),
		ScreeningID: screeningID,
		Seat:        seat,
		UserID:      userID,
		HoldToken:   uuid.NewString(),
		ExpiresAt:   expires,
	}
}

// TestSweepReclaimsExpiredHoldSlot is THE user-visible effect the sweeper
// exists for (ADR 0006: "the hold-limit slot back sooner"): a silent expiry
// leaks a limit slot until the sweep reconciles the bookkeeping.
func TestSweepReclaimsExpiredHoldSlot(t *testing.T) {
	ctx := context.Background()
	clock := memory.NewManualClock(epoch)
	store := memory.NewHoldStore(clock, 1) // one slot only
	sweeper := appholdexpiry.NewSweeper(store, 100, clock.Now)

	hold := newHold("alice", "scr-1", "A", 1, epoch.Add(holdTTL))
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	// Past expiry the seat frees itself (Redis TTL), but the LIMIT slot is
	// still occupied by the stale bookkeeping — the residue ADR 0002 documents.
	clock.Set(epoch.Add(holdTTL + time.Second))
	next := newHold("alice", "scr-1", "A", 2, clock.Now().Add(holdTTL))
	if err := store.Hold(ctx, next); !errors.Is(err, domain.ErrHoldLimitExceeded) {
		t.Fatalf("pre-sweep Hold err = %v, want ErrHoldLimitExceeded (slot leaked)", err)
	}

	swept, err := sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1", swept)
	}

	// The slot is back: the same user can hold again immediately.
	if err := store.Hold(ctx, next); err != nil {
		t.Errorf("post-sweep Hold: %v, want success (slot reclaimed)", err)
	}
}

func TestSweepLeavesLiveHoldsAlone(t *testing.T) {
	ctx := context.Background()
	clock := memory.NewManualClock(epoch)
	store := memory.NewHoldStore(clock, 4)
	sweeper := appholdexpiry.NewSweeper(store, 100, clock.Now)

	hold := newHold("bob", "scr-1", "B", 1, epoch.Add(holdTTL))
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	clock.Set(epoch.Add(holdTTL - time.Second)) // one second short of expiry
	swept, err := sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if swept != 0 {
		t.Errorf("swept = %d, want 0 (the hold is still live)", swept)
	}
	if _, err := store.Get(ctx, hold.SessionID); err != nil {
		t.Errorf("Get after sweep = %v, want the live hold intact", err)
	}
	seats, err := store.HeldSeats(ctx, "scr-1")
	if err != nil || len(seats) != 1 {
		t.Errorf("HeldSeats = %v, %v, want the one live seat", seats, err)
	}
}

// TestSweepBoundaryAtExpiresAt pins the inclusive boundary: the domain
// treats a hold as expired AT ExpiresAt (IsExpired: !now.Before(...)), and
// the sweep must agree — sweeping exactly at the boundary reclaims.
func TestSweepBoundaryAtExpiresAt(t *testing.T) {
	ctx := context.Background()
	clock := memory.NewManualClock(epoch)
	store := memory.NewHoldStore(clock, 4)
	sweeper := appholdexpiry.NewSweeper(store, 100, clock.Now)

	hold := newHold("cara", "scr-1", "C", 1, epoch.Add(holdTTL))
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	clock.Set(epoch.Add(holdTTL)) // EXACTLY the expiry instant
	swept, err := sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept at exactly ExpiresAt = %d, want 1 (domain expiry is inclusive)", swept)
	}
}

func TestSweepHonorsLimit(t *testing.T) {
	ctx := context.Background()
	clock := memory.NewManualClock(epoch)
	store := memory.NewHoldStore(clock, 10)
	sweeper := appholdexpiry.NewSweeper(store, 2, clock.Now) // limit 2 per pass

	for i := range 3 {
		hold := newHold("dave", "scr-1", "D", i+1, epoch.Add(holdTTL))
		if err := store.Hold(ctx, hold); err != nil {
			t.Fatalf("Hold %d: %v", i, err)
		}
	}
	clock.Set(epoch.Add(holdTTL + time.Second))

	swept, err := sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce #1: %v", err)
	}
	if swept != 2 {
		t.Errorf("first pass swept = %d, want 2 (limit)", swept)
	}
	swept, err = sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce #2: %v", err)
	}
	if swept != 1 {
		t.Errorf("second pass swept = %d, want the remaining 1", swept)
	}
}

// TestSweepGoneBranchReclaimsBookkeeping exercises the GONE path through
// the fake: observing the expiry first (Get) models Redis's TTL having
// already dropped the session hash, so the sweep finds only bookkeeping —
// and must still reclaim it (that IS the sweeper's common case in
// production, where hash, seat key, and score die together).
func TestSweepGoneBranchReclaimsBookkeeping(t *testing.T) {
	ctx := context.Background()
	clock := memory.NewManualClock(epoch)
	store := memory.NewHoldStore(clock, 1)
	sweeper := appholdexpiry.NewSweeper(store, 100, clock.Now)

	hold := newHold("erin", "scr-1", "E", 1, epoch.Add(holdTTL))
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	clock.Set(epoch.Add(holdTTL + time.Second))
	// Observe the expiry: the fake's lazy purge drops the session entry,
	// leaving exactly the Redis residue (bookkeeping without keys).
	if _, err := store.Get(ctx, hold.SessionID); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Fatalf("Get after expiry = %v, want ErrHoldNotFound", err)
	}

	swept, err := sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1 (GONE branch reclaims bookkeeping)", swept)
	}
	// Slot reclaimed: limit=1 and the next hold succeeds.
	if err := store.Hold(ctx, newHold("erin", "scr-1", "E", 2, clock.Now().Add(holdTTL))); err != nil {
		t.Errorf("post-sweep Hold: %v, want success", err)
	}
}

// TestSweepTokenCheckProtectsNewerHold is the ADR 0002 guarantee extended
// to the sweeper: between an old hold's expiry and its sweep, a NEW hold
// may claim the same seat — the sweep must reclaim the old bookkeeping
// without touching the newer hold's claim.
func TestSweepTokenCheckProtectsNewerHold(t *testing.T) {
	ctx := context.Background()
	clock := memory.NewManualClock(epoch)
	store := memory.NewHoldStore(clock, 4)
	sweeper := appholdexpiry.NewSweeper(store, 100, clock.Now)

	old := newHold("frank", "scr-1", "F", 1, epoch.Add(holdTTL))
	if err := store.Hold(ctx, old); err != nil {
		t.Fatalf("Hold old: %v", err)
	}

	// Time passes the old expiry; the seat frees itself (TTL), and a new
	// user claims it BEFORE the sweeper runs.
	clock.Set(epoch.Add(holdTTL + time.Second))
	fresh := newHold("gina", "scr-1", "F", 1, clock.Now().Add(holdTTL))
	if err := store.Hold(ctx, fresh); err != nil {
		t.Fatalf("re-Hold of the freed seat: %v", err)
	}

	swept, err := sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1 (only the expired hold)", swept)
	}
	// The newer hold is fully intact...
	got, err := store.Get(ctx, fresh.SessionID)
	if err != nil {
		t.Fatalf("Get(fresh): %v, want the newer hold alive", err)
	}
	if got.HoldToken != fresh.HoldToken {
		t.Errorf("fresh hold token = %q, want %q", got.HoldToken, fresh.HoldToken)
	}
	seats, err := store.HeldSeats(ctx, "scr-1")
	if err != nil || len(seats) != 1 || seats[0] != fresh.Seat {
		t.Errorf("HeldSeats = %v, %v, want exactly the fresh hold's seat", seats, err)
	}
	// ...and frank's slot is reclaimed (limit 4 makes this observable via
	// the bookkeeping count only indirectly; the real assertion is that the
	// fresh hold survived while exactly one sweep happened).
}

// failingStore is a one-method stub proving SweepOnce propagates store
// failures (no error injection needed on the fake for this).
type failingStore struct{ err error }

func (f failingStore) SweepExpired(context.Context, time.Time, int) (int, error) {
	return 0, f.err
}

func TestSweepOncePropagatesStoreError(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("redis down")
	sweeper := appholdexpiry.NewSweeper(failingStore{err: sentinel}, 100, time.Now)

	n, err := sweeper.SweepOnce(ctx)
	if !errors.Is(err, sentinel) {
		t.Errorf("SweepOnce err = %v, want it to wrap the store error", err)
	}
	if n != 0 {
		t.Errorf("swept = %d, want 0 on failure", n)
	}
}
