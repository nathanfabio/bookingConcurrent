package integration

import (
	"context"
	"testing"
	"time"

	redisadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/redis"

	goredis "github.com/redis/go-redis/v9"
)

func TestRedisActiveHoldCountUsesFutureScores(t *testing.T) {
	client := connectRedis(t)
	ctx := context.Background()
	store := redisadapter.NewHoldStore(client, time.Minute, 4)
	now := time.Now().Truncate(time.Second)
	// Isolated test Redis DB is flushed by connectRedis. Include an expired
	// entry and one exactly at the boundary: neither is an active checkout.
	if err := client.ZAdd(ctx, "holds:expiring",
		goredis.Z{Score: float64(now.Add(-time.Second).Unix()), Member: "alice:expired"},
		goredis.Z{Score: float64(now.Unix()), Member: "alice:boundary"},
		goredis.Z{Score: float64(now.Add(time.Second).Unix()), Member: "alice:future"},
	).Err(); err != nil {
		t.Fatal(err)
	}
	count, err := store.CountActiveHolds(ctx, now)
	if err != nil || count != 1 {
		t.Fatalf("active count=%d err=%v, want 1", count, err)
	}
	if cardinality := client.ZCard(ctx, "holds:expiring").Val(); cardinality != 3 {
		t.Fatalf("scrape must not mutate the index, cardinality=%d", cardinality)
	}
}
