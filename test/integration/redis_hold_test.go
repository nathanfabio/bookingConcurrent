// Package integration tests adapters against REAL infrastructure
// (CLAUDE.md §9): a concurrency test against real SET NX is the only way to
// prove the hold mechanism — racing the in-memory fake only proves the
// fake's mutex.
//
// Requirements: `make up` (compose provides Redis). If Redis is unreachable
// the tests SKIP with instructions rather than fail, so `go test ./...`
// stays runnable without infra; CI runs them against a service container.
//
// All tests run against a dedicated Redis DB index (TEST_REDIS_DB, default
// 14) which they FLUSH first — never against the dev DB, so manual holds
// made while developing survive the test suite.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	redisadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/redis"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// testHoldTTL is generous enough that no hold expires mid-assertion in the
// non-lifecycle tests.
const testHoldTTL = 5 * time.Minute

// connectRedis returns a client on the test DB (flushed), or skips the test.
func connectRedis(t *testing.T) *goredis.Client {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	db := 14
	if v := os.Getenv("TEST_REDIS_DB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 15 {
			t.Fatalf("TEST_REDIS_DB must be an integer in [0,15], got %q", v)
		}
		db = n
	}

	client := goredis.NewClient(&goredis.Options{Addr: addr, DB: db})
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not reachable at %s — start infra with `make up`: %v", addr, err)
	}

	// Dedicated test DB: safe to wipe.
	if err := client.FlushDB(ctx).Err(); nil != err {
		t.Fatalf("flush test DB %d: %v", db, err)
	}
	return client
}

func newStore(client *goredis.Client, maxHolds int) *redisadapter.HoldStore {
	return redisadapter.NewHoldStore(client, testHoldTTL, maxHolds)
}

// newHold builds a valid hold with fresh UUIDs, expiring after ttl.
func newHold(screeningID, row string, num int, userID string, ttl time.Duration) domain.Hold {
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
		ExpiresAt:   time.Now().Add(ttl),
	}
}

// TestConcurrentHoldExactlyOneWinner is THE test this project exists to
// pass: N goroutines race for the same seat; exactly one succeeds, and the
// resulting Redis state is clean — no orphaned session keys, bookkeeping
// matches the winner, the seat key carries the winner's token.
func TestConcurrentHoldExactlyOneWinner(t *testing.T) {
	client := connectRedis(t)
	store := newStore(client, 100)
	ctx := context.Background()

	const contenders = 32
	holds := make([]domain.Hold, contenders)
	for i := range holds {
		holds[i] = newHold("screening-1", "A", 1, fmt.Sprintf("user-%d", i), testHoldTTL)
	}

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		winners = make(chan domain.Hold, contenders)
	)
	errs := make([]error, contenders)

	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start // release all goroutines at once
			err := store.Hold(ctx, holds[n])
			errs[n] = err
			if err == nil {
				winners <- holds[n]
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(winners)

	var won []domain.Hold
	for h := range winners {
		won = append(won, h)
	}
	if len(won) != 1 {
		t.Fatalf("expected exactly 1 winner among %d contenders, got %d", contenders, len(won))
	}
	winner := won[0]

	// Every loser must have failed with the specific "already held" error —
	// not a transport error, not a panic, not nil.
	for i, err := range errs {
		if holds[i].SessionID == winner.SessionID {
			continue
		}
		if !errors.Is(err, domain.ErrSeatAlreadyHeld) {
			t.Errorf("contender %d: err = %v, want ErrSeatAlreadyHeld", i, err)
		}
	}

	// Cleanliness invariants: the atomic script must leave no residue.
	seatKey := "seat:screening-1:A:1"
	if got := client.Get(ctx, seatKey).Val(); got != winner.HoldToken {
		t.Errorf("seat key value = %q, want winner token %q", got, winner.HoldToken)
	}
	if n := client.ZCard(ctx, "holds:expiring").Val(); n != 1 {
		t.Errorf("global holds ZCARD = %d, want 1", n)
	}
	if n := client.ZCard(ctx, "user:"+winner.UserID+":holds").Val(); n != 1 {
		t.Errorf("winner's user ZCARD = %d, want 1", n)
	}
	for i := 0; i < contenders; i++ {
		if holds[i].SessionID == winner.SessionID {
			continue
		}
		if n := client.ZCard(ctx, "user:"+holds[i].UserID+":holds").Val(); n != 0 {
			t.Errorf("loser %d user ZCARD = %d, want 0 (no orphaned bookkeeping)", i, n)
		}
	}
	sessionCount := 0
	iter := client.Scan(ctx, 0, "session:*", 0).Iterator()
	for iter.Next(ctx) {
		sessionCount++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sessionCount != 1 {
		t.Errorf("found %d session keys, want exactly 1 (no orphaned sessions)", sessionCount)
	}

	// And the round trip: the winner's session resolves back to their hold.
	got, err := store.Get(ctx, winner.SessionID)
	if err != nil {
		t.Fatalf("Get(winner): %v", err)
	}
	if got.UserID != winner.UserID || got.HoldToken != winner.HoldToken {
		t.Errorf("Get returned %+v, want hold for %s", got, winner.UserID)
	}
}

// TestHoldExpiresAndSeatBecomesAvailableAgain is the lifecycle test CLAUDE.md
// §9 calls out specifically: hold → TTL expires → seat is claimable again.
// Expiry is Redis-native (EX), so this test really sleeps — kept as short as
// correctness allows.
func TestHoldExpiresAndSeatBecomesAvailableAgain(t *testing.T) {
	client := connectRedis(t)
	store := redisadapter.NewHoldStore(client, testHoldTTL, 10)
	ctx := context.Background()

	hold := newHold("screening-2", "B", 4, "alice", 1*time.Second)
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, err := store.Get(ctx, hold.SessionID); err != nil {
		t.Fatalf("Get immediately after Hold: %v", err)
	}

	// TTL 1s plus margin for scheduler jitter.
	time.Sleep(2200 * time.Millisecond)

	if _, err := store.Get(ctx, hold.SessionID); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("Get after expiry: err = %v, want ErrHoldNotFound", err)
	}

	// The core promise of TTL-based holds: nobody has to sweep, unlock, or
	// even notice — the next buyer simply succeeds.
	next := newHold("screening-2", "B", 4, "bob", testHoldTTL)
	if err := store.Hold(ctx, next); err != nil {
		t.Fatalf("re-Hold after expiry must succeed: %v", err)
	}

	// Known residue, reconciled by the hold-expiry sweeper milestone (the
	// one that introduces cmd/worker): the expired session's ZSET
	// bookkeeping outlives its keys (Redis expiry is silent). Until then it
	// is harmless — availability stays correct because seat keys expire on
	// their own (ADR 0006). Pin the current honest behavior so a future
	// change is deliberate.
	if n := client.ZCard(ctx, "user:alice:holds").Val(); n != 1 {
		t.Errorf("stale bookkeeping for alice = %d, want 1 until the sweeper runs", n)
	}
}

// TestHeldSeatsScansOnlyTheRequestedScreening proves the seat-map's held
// layer against real Redis: SCAN returns exactly the live seat keys of the
// requested screening, nothing from other screenings, and one corrupt key
// cannot sink the enumeration.
func TestHeldSeatsScansOnlyTheRequestedScreening(t *testing.T) {
	client := connectRedis(t)
	store := newStore(client, 10)
	ctx := context.Background()

	// Two holdings in screening-10, one in screening-11.
	for _, h := range []domain.Hold{
		newHold("screening-10", "A", 1, "alice", testHoldTTL),
		newHold("screening-10", "C", 3, "bob", testHoldTTL),
		newHold("screening-11", "A", 1, "carol", testHoldTTL),
	} {
		if err := store.Hold(ctx, h); err != nil {
			t.Fatalf("Hold %s: %v", h.SessionID, err)
		}
	}
	// A corrupt seat key in screening-10's keyspace: valid prefix, garbage
	// tail. The enumeration must skip it, not fail.
	if err := client.Set(ctx, "seat:screening-10:corrupt", "junk", testHoldTTL).Err(); err != nil {
		t.Fatalf("seed corrupt key: %v", err)
	}

	got, err := store.HeldSeats(ctx, "screening-10")
	if err != nil {
		t.Fatalf("HeldSeats: %v", err)
	}
	want := map[domain.Seat]bool{
		{Row: "A", Number: 1}: false,
		{Row: "C", Number: 3}: false,
	}
	if len(got) != len(want) {
		t.Fatalf("HeldSeats returned %d seats (%v), want %d", len(got), got, len(want))
	}
	for _, seat := range got {
		if _, ok := want[seat]; !ok {
			t.Errorf("unexpected seat %+v", seat)
		}
		want[seat] = true
	}
	for seat, seen := range want {
		if !seen {
			t.Errorf("missing seat %+v", seat)
		}
	}

	// A screening with no holds yields an empty slice, not an error.
	empty, err := store.HeldSeats(ctx, "screening-nothing")
	if err != nil || len(empty) != 0 {
		t.Errorf("empty screening: seats=%v err=%v, want none", empty, err)
	}
}

// TestLateReleaseCannotDestroyANewerHold proves the hold-token mechanism
// against real Redis: A's hold expires, B acquires the seat, and a late
// Release for A must leave B's claim completely intact.
func TestLateReleaseCannotDestroyANewerHold(t *testing.T) {
	client := connectRedis(t)
	store := redisadapter.NewHoldStore(client, testHoldTTL, 10)
	ctx := context.Background()

	a := newHold("screening-3", "C", 2, "alice", 1*time.Second)
	if err := store.Hold(ctx, a); err != nil {
		t.Fatalf("Hold A: %v", err)
	}
	time.Sleep(2200 * time.Millisecond) // A expires

	b := newHold("screening-3", "C", 2, "bob", testHoldTTL)
	if err := store.Hold(ctx, b); err != nil {
		t.Fatalf("Hold B: %v", err)
	}

	// A's session is gone, so Release fails at the first gate...
	if err := store.Release(ctx, a.SessionID, "alice"); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("late release: err = %v, want ErrHoldNotFound", err)
	}
	// ...and B's state is untouched: seat still carries B's token.
	if got := client.Get(ctx, "seat:screening-3:C:2").Val(); got != b.HoldToken {
		t.Errorf("seat value = %q, want B's token %q — a stale release must never evict a newer hold", got, b.HoldToken)
	}
	if got, err := store.Get(ctx, b.SessionID); err != nil || got.UserID != "bob" {
		t.Errorf("B's session damaged: %+v, %v", got, err)
	}
}

// TestReleaseOwnershipAndUnknownSessions covers the authorization behaviors
// the HTTP layer will map to 404s (CLAUDE.md §3: never leak session
// existence to non-owners).
func TestReleaseOwnershipAndUnknownSessions(t *testing.T) {
	client := connectRedis(t)
	store := newStore(client, 10)
	ctx := context.Background()

	hold := newHold("screening-4", "A", 9, "alice", testHoldTTL)
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	if err := store.Release(ctx, hold.SessionID, "mallory"); !errors.Is(err, domain.ErrNotHoldOwner) {
		t.Errorf("wrong-owner release: err = %v, want ErrNotHoldOwner", err)
	}
	if err := store.Release(ctx, "no-such-session", "alice"); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("unknown session release: err = %v, want ErrHoldNotFound", err)
	}
	// Alice's hold survived both attempts.
	if _, err := store.Get(ctx, hold.SessionID); err != nil {
		t.Errorf("alice's hold damaged: %v", err)
	}

	// The happy path still works.
	if err := store.Release(ctx, hold.SessionID, "alice"); err != nil {
		t.Fatalf("owner release: %v", err)
	}
	if n := client.Exists(ctx, "seat:screening-4:A:9").Val(); n != 0 {
		t.Error("seat key must be gone after release")
	}
	if n := client.ZCard(ctx, "holds:expiring").Val(); n != 0 {
		t.Errorf("global ZCARD after release = %d, want 0", n)
	}
}

// TestHoldLimitEnforcedAtomically: the per-user cap is checked INSIDE the
// Lua script, so concurrent requests from one user must never exceed it —
// a check-then-act from Go would let the burst through.
func TestHoldLimitEnforcedAtomically(t *testing.T) {
	client := connectRedis(t)
	const limit = 2
	store := newStore(client, limit)
	ctx := context.Background()

	// Sequential sanity: third hold is rejected.
	var held []domain.Hold
	for i := 0; i < 3; i++ {
		h := newHold("screening-5", "D", i+1, "alice", testHoldTTL)
		err := store.Hold(ctx, h)
		if i < limit && err != nil {
			t.Fatalf("hold %d within limit failed: %v", i, err)
		}
		if i >= limit && !errors.Is(err, domain.ErrHoldLimitExceeded) {
			t.Fatalf("hold %d beyond limit: err = %v, want ErrHoldLimitExceeded", i, err)
		}
		if err == nil {
			held = append(held, h)
		}
	}

	// Concurrent burst: release first, then fire 10 goroutines at distinct
	// seats as the same user. Exactly `limit` may win.
	for _, h := range held {
		if err := store.Release(ctx, h.SessionID, "alice"); err != nil {
			t.Fatalf("cleanup release: %v", err)
		}
	}

	const burst = 10
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		success int
	)
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			h := newHold("screening-5", "E", n+1, "alice", testHoldTTL)
			if store.Hold(ctx, h) == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if success != limit {
		t.Errorf("burst of %d with limit %d: %d succeeded, want exactly %d (limit must be atomic)", burst, limit, success, limit)
	}
}
