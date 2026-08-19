// Package postgres is the durable-store adapter (CLAUDE.md §2): a pgx
// connection pool with tracing, goose migrations, sqlc-generated queries,
// and thin repositories that map rows to domain types.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool builds the pgx connection pool from validated config and verifies
// the database is reachable before returning.
//
// Failing the ping at construction (instead of lazily on first query) keeps
// the boot contract honest: an API that cannot reach its source of truth
// should say so immediately — /readyz then reports the same state.
func NewPool(ctx context.Context, cfg config.Postgres) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}

	// Trace every query through OpenTelemetry. Spans go to the console
	// exporter until M8 points them at Jaeger; the instrumentation itself
	// is in place from the day the adapter exists.
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer()

	// Bound how long an acquisition can sit before we call the pool wedged.
	poolCfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}
