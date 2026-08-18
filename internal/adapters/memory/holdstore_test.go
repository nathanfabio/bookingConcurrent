package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

var epoch = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func newTestHold(sessionID, screeningID, row string, num int, userID, token string, expires time.Time) domain.Hold {
	seat, err := domain.NewSeat(row, num)
	if err != nil {
		panic(err)
	}
	return domain.Hold{
		SessionID:   sessionID,
		ScreeningID: screeningID,
		Seat:        seat,
		UserID:      userID,
		HoldToken:   token,
		ExpiresAt:   expires,
	}
}

func TestFakeHoldReleaseLifecycle(t *testing.T) {
	store := NewHoldStore(NewManualClock(epoch), 4)
	ctx := context.Background()

	hold := newTestHold("s1", "scr1", "A", 1, "alice", "tok-1", epoch.Add(5*time.Minute))
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	got, err := store.Get(ctx, "s1")
	if err != nil || got.HoldToken != "tok-1" {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	if err := store.Release(ctx, "s1", "alice"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := store.Get(ctx, "s1"); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("after release, Get err = %v, want ErrHoldNotFound", err)
	}
	// Seat is claimable again immediately after release.
	again := newTestHold("s2", "scr1", "A", 1, "bob", "tok-2", epoch.Add(5*time.Minute))
	if err := store.Hold(ctx, again); err != nil {
		t.Fatalf("re-Hold after release: %v", err)
	}
}

func TestFakeExpiryViaClock(t *testing.T) {
	clock := NewManualClock(epoch)
	store := NewHoldStore(clock, 4)
	ctx := context.Background()

	hold := newTestHold("s1", "scr1", "A", 1, "alice", "tok-1", epoch.Add(2*time.Second))
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	clock.Advance(2 * time.Second) // exactly at TTL: expired (inclusive)

	if _, err := store.Get(ctx, "s1"); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("expired Get err = %v, want ErrHoldNotFound", err)
	}
	if err := store.Release(ctx, "s1", "alice"); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("expired Release err = %v, want ErrHoldNotFound", err)
	}

	// Silent expiry frees the seat for the next claimant — the core
	// hold→expire→rehold lifecycle, tested without any sleeping.
	again := newTestHold("s2", "scr1", "A", 1, "bob", "tok-2", clock.Now().Add(2*time.Second))
	if err := store.Hold(ctx, again); err != nil {
		t.Fatalf("re-Hold after expiry: %v", err)
	}
}

func TestFakeStaleBookkeepingCountsTowardLimitUntilSwept(t *testing.T) {
	// Faithful to Redis: expiry is silent, so the user's ZSET-equivalent
	// keeps the stale member until the sweeper reconciles it (M4). This
	// test pins that behavior so nobody "fixes" the fake into diverging
	// from the real store.
	clock := NewManualClock(epoch)
	store := NewHoldStore(clock, 1)
	ctx := context.Background()

	first := newTestHold("s1", "scr1", "A", 1, "alice", "tok-1", epoch.Add(time.Second))
	if err := store.Hold(ctx, first); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	clock.Advance(2 * time.Second)

	second := newTestHold("s2", "scr1", "A", 2, "alice", "tok-2", clock.Now().Add(time.Minute))
	if err := store.Hold(ctx, second); !errors.Is(err, domain.ErrHoldLimitExceeded) {
		t.Errorf("limit with stale member: err = %v, want ErrHoldLimitExceeded", err)
	}

	// Releasing the (expired) session is impossible — it is already gone —
	// so reconciliation must come from outside. The sweeper owns that in
	// production; in this test we only assert the store-level truth.
}

func TestFakeReleaseTokenSafety(t *testing.T) {
	// The scenario hold tokens exist for: A's hold expires, B acquires the
	// same seat, then A's stale release arrives. B's claim must survive.
	clock := NewManualClock(epoch)
	store := NewHoldStore(clock, 4)
	ctx := context.Background()

	a := newTestHold("sA", "scr1", "A", 1, "alice", "tok-A", epoch.Add(time.Second))
	if err := store.Hold(ctx, a); err != nil {
		t.Fatalf("Hold A: %v", err)
	}
	clock.Advance(2 * time.Second)

	b := newTestHold("sB", "scr1", "A", 1, "bob", "tok-B", clock.Now().Add(time.Minute))
	if err := store.Hold(ctx, b); err != nil {
		t.Fatalf("Hold B: %v", err)
	}

	// A's release fails at the ownership/expiry gate, but even a
	// hypothetical bypass must not touch B's seat: the token comparison is
	// the second line of defense. Here we assert the observable outcome.
	if err := store.Release(ctx, "sA", "alice"); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("stale release err = %v, want ErrHoldNotFound", err)
	}
	got, err := store.Get(ctx, "sB")
	if err != nil || got.HoldToken != "tok-B" {
		t.Errorf("B's hold must survive A's stale release: %+v, %v", got, err)
	}
}

func TestFakeOwnershipChecks(t *testing.T) {
	store := NewHoldStore(NewManualClock(epoch), 4)
	ctx := context.Background()

	hold := newTestHold("s1", "scr1", "A", 1, "alice", "tok-1", epoch.Add(time.Minute))
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	if err := store.Release(ctx, "s1", "mallory"); !errors.Is(err, domain.ErrNotHoldOwner) {
		t.Errorf("wrong-owner release err = %v, want ErrNotHoldOwner", err)
	}
	if _, err := store.Get(ctx, "no-such-session"); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("unknown session err = %v, want ErrHoldNotFound", err)
	}
	// Alice's hold must be untouched by the failed attempts.
	if _, err := store.Get(ctx, "s1"); err != nil {
		t.Errorf("alice's hold damaged by failed release: %v", err)
	}
}

func TestFakeConcurrentRaceOneWinner(t *testing.T) {
	// The port contract under load, proven against the fake's mutex. The
	// integration suite proves the same against real Redis SET NX.
	const contenders = 64
	store := NewHoldStore(NewManualClock(epoch), contenders)
	ctx := context.Background()

	var (
		wg        sync.WaitGroup
		start     = make(chan struct{})
		successes = make(chan string, contenders)
	)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			user := fmt.Sprintf("user-%d", n)
			h := newTestHold(
				fmt.Sprintf("session-%d", n), "scr1", "A", 1,
				user, fmt.Sprintf("tok-%d", n), epoch.Add(5*time.Minute),
			)
			if err := store.Hold(ctx, h); err == nil {
				successes <- user
			} else if !errors.Is(err, domain.ErrSeatAlreadyHeld) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(successes)

	var winners []string
	for u := range successes {
		winners = append(winners, u)
	}
	if len(winners) != 1 {
		t.Fatalf("expected exactly one winner, got %d: %v", len(winners), winners)
	}
}
