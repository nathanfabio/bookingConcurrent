package postgres

import (
	"context"
	"fmt"
	"time"

	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
	domainuser "github.com/nathanfabio/bookingConcurrent/internal/domain/user"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedDev populates a development database with a small, fixed catalog
// and one login-able user so `make run` is usable immediately. It is
// idempotent (skips when movies exist) and MUST only be called from
// development wiring — CI and production compose their own data through
// migrations + explicit steps.
//
// hashPassword is injected (the composition root passes the argon2id
// hasher) so this adapter never imports application/auth — the same
// dependency-direction rule the middleware follows (CLAUDE.md §1).
func SeedDev(ctx context.Context, pool *pgxpool.Pool, hashPassword func(string) (string, error)) error {
	movies := NewMovieRepo(pool)
	screenings := NewScreeningRepo(pool)

	existing, err := movies.List(ctx)
	if err != nil {
		return fmt.Errorf("seed: list movies: %w", err)
	}
	if len(existing) > 0 {
		return nil // already seeded
	}

	if err := seedDevUser(ctx, pool, hashPassword); err != nil {
		return err
	}

	catalog := []struct {
		movie     domainmovie.Movie
		screening []time.Duration // offsets from now for each screening
	}{
		{
			movie:     domainmovie.Movie{Title: "The Concurrency Menace", Synopsis: "A race condition threatens a small town's only cinema.", DurationMinutes: 112},
			screening: []time.Duration{24 * time.Hour, 48 * time.Hour},
		},
		{
			movie:     domainmovie.Movie{Title: "Eventual Consistency", Synopsis: "Two replicas learn that love, like writes, takes time to propagate.", DurationMinutes: 96},
			screening: []time.Duration{30 * time.Hour},
		},
	}

	// Geometry: 6 rows (A-F), 10 seats each. Small enough to eyeball in
	// redis-commander and seat-map responses, large enough for races.
	rows := []string{"A", "B", "C", "D", "E", "F"}
	const seatsPerRow = 10

	for _, item := range catalog {
		created, err := movies.Create(ctx, item.movie)
		if err != nil {
			return fmt.Errorf("seed: create movie %q: %w", item.movie.Title, err)
		}
		for _, offset := range item.screening {
			_, err := screenings.Create(ctx, domainmovie.Screening{
				MovieID:     created.ID,
				StartsAt:    time.Now().Add(offset).Truncate(time.Minute),
				Rows:        rows,
				SeatsPerRow: seatsPerRow,
			})
			if err != nil {
				return fmt.Errorf("seed: create screening: %w", err)
			}
		}
	}
	return nil
}

// seedDevUser inserts a login-able development account so `curl POST
// /auth/login` works the moment the stack is up. The password is a
// DOCUMENTED NON-SECRET shared with compose.yaml and .env.example — it
// exists only so a dev box is usable, never to protect anything.
//
//nolint:gosec // G101: this is a documented throwaway dev credential, not a secret.
const devUserPassword = "booking-dev-password"

func seedDevUser(ctx context.Context, pool *pgxpool.Pool, hashPassword func(string) (string, error)) error {
	users := NewUserRepo(pool)
	hash, err := hashPassword(devUserPassword)
	if err != nil {
		return fmt.Errorf("seed: hash dev password: %w", err)
	}
	_, err = users.Create(ctx, domainuser.User{
		Email:        "dev@example.com",
		PasswordHash: hash,
		DisplayName:  "Dev User",
		Role:         domainuser.RoleCustomer,
	})
	if err != nil {
		return fmt.Errorf("seed: create dev user: %w", err)
	}
	return nil
}
