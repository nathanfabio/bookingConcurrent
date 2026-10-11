package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	redisadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/redis"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// The sweeper tests reuse the lifecycle timing pattern pinned by
// TestHoldExpiresAndSeatBecomesAvailableAgain: a 1s hold TTL plus a 2.2s
// sleep guarantees the keys are gone (GONE branch) while the ZSET members —
// which have no TTL — remain for the sweep to reclaim.

// TestGlobalHoldZSETMemberIsComposite pins the ADR 0010 schema refinement:
// the GLOBAL ZSET member carries "userID:sessionID" (the sweeper's only
// route to the per-user set once the session hash has TTL-expired), while
// the USER ZSET member stays the bare sessionID (its key already names the
// user). Renaming this format is a migration-shaped decision, so it breaks
// the build here instead of silently stranding bookkeeping.
func TestGlobalHoldZSETMemberIsComposite(t *testing.T) {
	client := connectRedis(t)
	store := newStore(client, 10)
	ctx := context.Background()

	hold := newHold("screening-20", "A", 1, "alice", testHoldTTL)
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	global := client.ZRange(ctx, "holds:expiring", 0, -1).Val()
	if len(global) != 1 || global[0] != "alice:"+hold.SessionID {
		t.Errorf("global members = %v, want exactly [alice:%s]", global, hold.SessionID)
	}
	user := client.ZRange(ctx, "user:alice:holds", 0, -1).Val()
	if len(user) != 1 || user[0] != hold.SessionID {
		t.Errorf("user members = %v, want exactly [%s] (bare sessionID)", user, hold.SessionID)
	}
	// The score is the truncated-Unix expiry both the sweep window and the
	// domain's IsExpired agree on.
	if score := client.ZScore(ctx, "holds:expiring", global[0]).Val(); int64(score) != hold.ExpiresAt.Unix() {
		t.Errorf("score = %v, want %d", score, hold.ExpiresAt.Unix())
	}
}

// TestSweepReclaimsExpiredHoldSlot is the sweeper's reason to exist, end to
// end against real Redis: a silent expiry leaks the user's hold-limit slot
// (stale ZSET member), the sweep reclaims it, and the user can hold again —
// with the bookkeeping verifiably gone from BOTH ZSETs.
func TestSweepReclaimsExpiredHoldSlot(t *testing.T) {
	client := connectRedis(t)
	store := redisadapter.NewHoldStore(client, testHoldTTL, 1) // ONE slot
	ctx := context.Background()

	hold := newHold("screening-21", "A", 1, "alice", 1*time.Second)
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	time.Sleep(2200 * time.Millisecond) // TTL 1s + scheduler margin

	// The leak, proven against the real in-script ZCARD: alice's slot is
	// occupied by a member whose keys no longer exist.
	retry := newHold("screening-21", "A", 2, "alice", testHoldTTL)
	if err := store.Hold(ctx, retry); !errors.Is(err, domain.ErrHoldLimitExceeded) {
		t.Fatalf("pre-sweep Hold err = %v, want ErrHoldLimitExceeded (the leak)", err)
	}

	swept, err := store.SweepExpired(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1", swept)
	}
	if n := client.ZCard(ctx, "user:alice:holds").Val(); n != 0 {
		t.Errorf("alice's user ZCARD after sweep = %d, want 0", n)
	}
	if n := client.ZCard(ctx, "holds:expiring").Val(); n != 0 {
		t.Errorf("global ZCARD after sweep = %d, want 0", n)
	}
	// The slot is back — this is the user-visible effect.
	if err := store.Hold(ctx, retry); err != nil {
		t.Errorf("post-sweep Hold: %v, want success (slot reclaimed)", err)
	}
}

// TestSweepTokenCheckProtectsNewerHold extends
// TestLateReleaseCannotDestroyANewerHold to the sweeper, fulfilling ADR
// 0002's promise that it "follows the same rule": when a seat is re-claimed
// between an old hold's expiry and its sweep, the sweep reclaims only the
// old bookkeeping and leaves the NEW hold's every trace intact.
func TestSweepTokenCheckProtectsNewerHold(t *testing.T) {
	client := connectRedis(t)
	store := newStore(client, 10)
	ctx := context.Background()

	old := newHold("screening-22", "A", 1, "alice", 1*time.Second)
	if err := store.Hold(ctx, old); err != nil {
		t.Fatalf("Hold old: %v", err)
	}
	time.Sleep(2200 * time.Millisecond)

	fresh := newHold("screening-22", "A", 1, "bob", testHoldTTL)
	if err := store.Hold(ctx, fresh); err != nil {
		t.Fatalf("re-Hold of the freed seat: %v", err)
	}

	swept, err := store.SweepExpired(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want exactly 1 (alice's expired hold)", swept)
	}

	// Bob's hold is untouched in EVERY dimension.
	seatKey := "seat:screening-22:A:1"
	if token := client.Get(ctx, seatKey).Val(); token != fresh.HoldToken {
		t.Errorf("seat token = %q, want bob's %q (sweep must not touch a newer claim)", token, fresh.HoldToken)
	}
	if _, err := store.Get(ctx, fresh.SessionID); err != nil {
		t.Errorf("Get(bob's session): %v, want it alive", err)
	}
	if n := client.ZCard(ctx, "user:bob:holds").Val(); n != 1 {
		t.Errorf("bob's user ZCARD = %d, want 1", n)
	}
	global := client.ZRange(ctx, "holds:expiring", 0, -1).Val()
	if len(global) != 1 || global[0] != "bob:"+fresh.SessionID {
		t.Errorf("global members = %v, want exactly bob's live hold", global)
	}
	// Alice's residue is fully gone.
	if n := client.ZCard(ctx, "user:alice:holds").Val(); n != 0 {
		t.Errorf("alice's user ZCARD = %d, want 0", n)
	}
}

// TestSweepTeardownWhenSessionStillPresent exercises the SWEPT branch
// deterministically: the ceiling-rounded TTL can keep the hash alive past
// its expiry score (ADR 0002's conservative Redis), and the sweep must then
// do the FULL token-checked teardown — seat key, session hash, both ZSETs.
// The inconsistency is hand-crafted (hash field + score set into the past)
// because waiting for the sub-second window would be flaky.
func TestSweepTeardownWhenSessionStillPresent(t *testing.T) {
	client := connectRedis(t)
	store := newStore(client, 10)
	ctx := context.Background()

	hold := newHold("screening-23", "C", 3, "alice", testHoldTTL)
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	past := time.Now().Add(-5 * time.Second)
	member := "alice:" + hold.SessionID
	sessionKey := "session:" + hold.SessionID
	if err := client.HSet(ctx, sessionKey, "expires_at", past.Unix()).Err(); err != nil {
		t.Fatalf("age the session hash: %v", err)
	}
	if err := client.ZAdd(ctx, "holds:expiring", goredis.Z{Score: float64(past.Unix()), Member: member}).Err(); err != nil {
		t.Fatalf("age the global score: %v", err)
	}

	swept, err := store.SweepExpired(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1 (SWEPT branch)", swept)
	}
	// Full teardown: seat key deleted (its token matched), hash deleted,
	// both ZSETs clean.
	if n := client.Exists(ctx, "seat:screening-23:C:3").Val(); n != 0 {
		t.Error("seat key survived a token-matched sweep")
	}
	if n := client.Exists(ctx, sessionKey).Val(); n != 0 {
		t.Error("session hash survived the sweep")
	}
	if n := client.ZCard(ctx, "user:alice:holds").Val(); n != 0 {
		t.Errorf("alice's user ZCARD = %d, want 0", n)
	}
	if n := client.ZCard(ctx, "holds:expiring").Val(); n != 0 {
		t.Errorf("global ZCARD = %d, want 0", n)
	}
}

// TestSweepSkipsAliveHoldWithStaleScore exercises the defensive ALIVE
// branch: when the score window and the hash DISAGREE (only hand-tampered
// data reaches this state), the sweep mutates NOTHING — an inconsistent
// member is never a license to delete live state.
func TestSweepSkipsAliveHoldWithStaleScore(t *testing.T) {
	client := connectRedis(t)
	store := newStore(client, 10)
	ctx := context.Background()

	hold := newHold("screening-24", "D", 4, "alice", testHoldTTL) // expires in 5m
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// Forge ONLY the score into the past; the hash keeps its live expiry.
	member := "alice:" + hold.SessionID
	past := time.Now().Add(-5 * time.Second)
	if err := client.ZAdd(ctx, "holds:expiring", goredis.Z{Score: float64(past.Unix()), Member: member}).Err(); err != nil {
		t.Fatalf("forge stale score: %v", err)
	}

	swept, err := store.SweepExpired(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if swept != 0 {
		t.Errorf("swept = %d, want 0 (ALIVE: hash says live, never mutate on disagreement)", swept)
	}
	if _, err := store.Get(ctx, hold.SessionID); err != nil {
		t.Errorf("Get: %v, want the live hold intact", err)
	}
	if n := client.Exists(ctx, "seat:screening-24:D:4").Val(); n != 1 {
		t.Error("seat key must survive the skipped sweep")
	}
	if n := client.ZCard(ctx, "user:alice:holds").Val(); n != 1 {
		t.Errorf("alice's user ZCARD = %d, want 1", n)
	}
	if n := client.ZCard(ctx, "holds:expiring").Val(); n != 1 {
		t.Errorf("global ZCARD = %d, want 1 (the member waits for its real expiry)", n)
	}
}

func TestSweepHonorsLimit(t *testing.T) {
	client := connectRedis(t)
	store := newStore(client, 10)
	ctx := context.Background()

	for i := range 3 {
		hold := newHold("screening-25", "E", i+1, "alice", 1*time.Second)
		if err := store.Hold(ctx, hold); err != nil {
			t.Fatalf("Hold %d: %v", i, err)
		}
	}
	time.Sleep(2200 * time.Millisecond)

	swept, err := store.SweepExpired(ctx, time.Now(), 2)
	if err != nil {
		t.Fatalf("SweepExpired #1: %v", err)
	}
	if swept != 2 {
		t.Errorf("first pass swept = %d, want 2 (limit)", swept)
	}
	if n := client.ZCard(ctx, "holds:expiring").Val(); n != 1 {
		t.Errorf("global ZCARD between passes = %d, want the remaining 1", n)
	}
	swept, err = store.SweepExpired(ctx, time.Now(), 2)
	if err != nil {
		t.Fatalf("SweepExpired #2: %v", err)
	}
	if swept != 1 {
		t.Errorf("second pass swept = %d, want 1", swept)
	}
	if n := client.ZCard(ctx, "holds:expiring").Val(); n != 0 {
		t.Errorf("global ZCARD after draining = %d, want 0", n)
	}
}

// TestSweepDiscardsUnparseableMember proves the anti-clog path: a member
// without the composite separator (corrupt data, or a pre-M6 legacy member
// written before the schema refinement) is discarded loudly instead of
// re-entering every LIMIT window forever — and a legitimate expired member
// in the same window is still swept.
func TestSweepDiscardsUnparseableMember(t *testing.T) {
	client := connectRedis(t)
	store := newStore(client, 10)
	ctx := context.Background()

	// A legacy-shaped member: bare sessionID UUID, no colon (exactly what
	// pre-M6 holds wrote). Its per-user entry is unrecoverable — the
	// documented upgrade cost in ADR 0010.
	legacy := uuid.NewString()
	past := float64(time.Now().Add(-5 * time.Second).Unix())
	if err := client.ZAdd(ctx, "holds:expiring", goredis.Z{Score: past, Member: legacy}).Err(); err != nil {
		t.Fatalf("seed legacy member: %v", err)
	}
	hold := newHold("screening-26", "F", 1, "alice", 1*time.Second)
	if err := store.Hold(ctx, hold); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	time.Sleep(2200 * time.Millisecond)

	swept, err := store.SweepExpired(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1 (only the parseable member counts)", swept)
	}
	if n := client.ZCard(ctx, "holds:expiring").Val(); n != 0 {
		t.Errorf("global ZCARD = %d, want 0 (legacy member discarded, expired member swept)", n)
	}
	if n := client.ZScore(ctx, "holds:expiring", legacy).Err(); !errors.Is(n, goredis.Nil) {
		t.Errorf("legacy member still present: %v", n)
	}
}
