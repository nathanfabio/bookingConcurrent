package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MovieRepo maps between generated rows and the movie domain model. It is
// deliberately thin: no query logic lives here, only translation. Anything
// resembling a decision belongs to the use case that calls it.
type MovieRepo struct {
	q *sqlcgen.Queries
}

// NewMovieRepo builds the repository over the shared pool.
func NewMovieRepo(pool *pgxpool.Pool) *MovieRepo {
	return &MovieRepo{q: sqlcgen.New(pool)}
}

// Create inserts a new movie; the database assigns id and created_at.
func (r *MovieRepo) Create(ctx context.Context, m domainmovie.Movie) (domainmovie.Movie, error) {
	row, err := r.q.CreateMovie(ctx, sqlcgen.CreateMovieParams{
		Title:           m.Title,
		Synopsis:        m.Synopsis,
		DurationMinutes: int32(m.DurationMinutes),
	})
	if err != nil {
		return domainmovie.Movie{}, fmt.Errorf("movie: create: %w", err)
	}
	return movieFromRow(row), nil
}

// Get loads one movie by id.
func (r *MovieRepo) Get(ctx context.Context, id string) (domainmovie.Movie, error) {
	row, err := r.q.GetMovie(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domainmovie.Movie{}, domainmovie.ErrMovieNotFound
	}
	if err != nil {
		return domainmovie.Movie{}, fmt.Errorf("movie: get: %w", err)
	}
	return movieFromRow(row), nil
}

// List returns all movies ordered by title.
func (r *MovieRepo) List(ctx context.Context) ([]domainmovie.Movie, error) {
	rows, err := r.q.ListMovies(ctx)
	if err != nil {
		return nil, fmt.Errorf("movie: list: %w", err)
	}
	movies := make([]domainmovie.Movie, 0, len(rows))
	for _, row := range rows {
		movies = append(movies, movieFromRow(row))
	}
	return movies, nil
}

func movieFromRow(row sqlcgen.Movie) domainmovie.Movie {
	return domainmovie.Movie{
		ID:              row.ID,
		Title:           row.Title,
		Synopsis:        row.Synopsis,
		DurationMinutes: int(row.DurationMinutes),
		CreatedAt:       ts(row.CreatedAt),
	}
}

// ScreeningRepo maps screening rows, including the seat-grid geometry the
// hold validation and seat map depend on.
type ScreeningRepo struct {
	q *sqlcgen.Queries
}

// NewScreeningRepo builds the repository over the shared pool.
func NewScreeningRepo(pool *pgxpool.Pool) *ScreeningRepo {
	return &ScreeningRepo{q: sqlcgen.New(pool)}
}

// Create inserts a screening for an existing movie.
func (r *ScreeningRepo) Create(ctx context.Context, s domainmovie.Screening) (domainmovie.Screening, error) {
	row, err := r.q.CreateScreening(ctx, sqlcgen.CreateScreeningParams{
		MovieID:     s.MovieID,
		StartsAt:    pgtype.Timestamptz{Time: s.StartsAt, Valid: true},
		Rows:        s.Rows,
		SeatsPerRow: int32(s.SeatsPerRow),
		PriceCents:  int32(s.PriceCents),
	})
	if err != nil {
		return domainmovie.Screening{}, fmt.Errorf("screening: create: %w", err)
	}
	return screeningFromRow(row), nil
}

// Get loads one screening by id.
func (r *ScreeningRepo) Get(ctx context.Context, id string) (domainmovie.Screening, error) {
	row, err := r.q.GetScreening(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domainmovie.Screening{}, domainmovie.ErrScreeningNotFound
	}
	if err != nil {
		return domainmovie.Screening{}, fmt.Errorf("screening: get: %w", err)
	}
	return screeningFromRow(row), nil
}

// ListByMovie returns a movie's screenings in chronological order.
func (r *ScreeningRepo) ListByMovie(ctx context.Context, movieID string) ([]domainmovie.Screening, error) {
	rows, err := r.q.ListScreeningsByMovie(ctx, movieID)
	if err != nil {
		return nil, fmt.Errorf("screening: list: %w", err)
	}
	screenings := make([]domainmovie.Screening, 0, len(rows))
	for _, row := range rows {
		screenings = append(screenings, screeningFromRow(row))
	}
	return screenings, nil
}

func screeningFromRow(row sqlcgen.Screening) domainmovie.Screening {
	return domainmovie.Screening{
		ID:          row.ID,
		MovieID:     row.MovieID,
		StartsAt:    ts(row.StartsAt),
		Rows:        row.Rows,
		SeatsPerRow: int(row.SeatsPerRow),
		PriceCents:  int(row.PriceCents),
		CreatedAt:   ts(row.CreatedAt),
	}
}

// ts unwraps a pgx timestamptz. Schema timestamps are all NOT NULL
// DEFAULT now(), so an invalid value here means a corrupt row — we return
// the zero time and let callers treat it as data corruption, not an
// expected branch.
func ts(t pgtype.Timestamptz) time.Time {
	return t.Time
}
