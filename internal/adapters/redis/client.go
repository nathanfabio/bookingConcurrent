// Package redis implements the hold store against a real Redis instance
// (CLAUDE.md §2): SET NX + TTL as the atomic, self-expiring seat hold,
// with a session reverse-lookup key and Lua scripts for every multi-key
// operation (ADR 0002).
package redis

import (
	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"

	goredis "github.com/redis/go-redis/v9"
)

// NewClient builds the Redis client from validated config.
//
// No retry loop here on purpose: if Redis is unreachable at boot the
// process fails readiness (M2 wires /readyz to this Ping) and the operator
// sees it immediately. Connection-level retries happen inside go-redis per
// command; a boot-time reconnect loop would only hide outages.
func NewClient(cfg config.Redis) *goredis.Client {
	return goredis.NewClient(&goredis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		// PoolSize stays at the default (10 per CPU): holds are one-round-trip
		// Lua scripts, so the client multiplexes well without tuning.
	})
}
