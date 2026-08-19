package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
	"github.com/nathanfabio/bookingConcurrent/migrations"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	// goose speaks database/sql; pgx's stdlib shim is how it reaches
	// Postgres without adding a second driver to the codebase.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// MigrateUp applies all pending migrations from the embedded filesystem.
//
// Who calls it (CLAUDE.md §2):
//   - development: cmd/api runs it automatically at boot — fast iteration,
//     and a dev box can never be silently un-migrated;
//   - CI and anything resembling production: run `go run ./cmd/migrate up`
//     as an explicit step, so a schema change is a visible, reviewable
//     deploy action rather than a side effect of starting a process.
//
// goose records applied versions in its own table, so this is idempotent.
func MigrateUp(ctx context.Context, cfg config.Postgres) (int, error) {
	db, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return 0, fmt.Errorf("migrate: open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(dialect(), db, migrations.FS,
		goose.WithLogger(goose.NopLogger()))
	if err != nil {
		return 0, fmt.Errorf("migrate: provider: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("migrate: up: %w", err)
	}
	return len(results), nil
}

// MigrateStatus reports applied vs pending migration versions — used by
// `go run ./cmd/migrate status` to inspect state without changing it.
func MigrateStatus(ctx context.Context, cfg config.Postgres) (string, error) {
	db, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return "", fmt.Errorf("migrate: open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(dialect(), db, migrations.FS,
		goose.WithLogger(goose.NopLogger()))
	if err != nil {
		return "", fmt.Errorf("migrate: provider: %w", err)
	}

	statuses, err := provider.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("migrate: status: %w", err)
	}
	out := ""
	for _, s := range statuses {
		out += fmt.Sprintf("%-8s\t%d\t%s\n", s.State, s.Source.Version, s.Source.Path)
	}
	return out, nil
}

func dialect() database.Dialect { return database.DialectPostgres }
