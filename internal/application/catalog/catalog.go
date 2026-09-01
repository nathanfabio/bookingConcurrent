// Package catalog contains the read-only catalog use cases (movies and
// their screenings) and the ports they depend on. Like booking, ports live
// here next to the code that calls them (ADR 0001).
//
// These endpoints are the client's way to discover screening IDs — without
// them the booking flow has no addressable seat maps. They are public
// reads with no business logic beyond existence checks.
package catalog

import (
	"context"
	"fmt"

	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
)

// MovieStore loads catalog reference data. Only the reads the catalog use
// cases need are on the interface; Create lives on the Postgres repo for
// the dev seed and is not a port concern.
type MovieStore interface {
	// List returns every movie, ordered by title.
	List(ctx context.Context) ([]domainmovie.Movie, error)
	// Get loads one movie by id. Errors: domainmovie.ErrMovieNotFound.
	Get(ctx context.Context, id string) (domainmovie.Movie, error)
}

// ScreeningLister lists a movie's screenings chronologically.
type ScreeningLister interface {
	ListByMovie(ctx context.Context, movieID string) ([]domainmovie.Screening, error)
}

// Service orchestrates the catalog reads. It depends only on ports so unit
// tests run against in-memory fakes and production runs against Postgres.
type Service struct {
	movies     MovieStore
	screenings ScreeningLister
}

// NewService wires the catalog use cases.
func NewService(movies MovieStore, screenings ScreeningLister) *Service {
	return &Service{movies: movies, screenings: screenings}
}

// Movies returns the full catalog, ordered by title.
func (s *Service) Movies(ctx context.Context) ([]domainmovie.Movie, error) {
	list, err := s.movies.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalog: list movies: %w", err)
	}
	return list, nil
}

// Screenings returns a movie's showings. An unknown movie is a 404, not an
// empty list — the existence check before listing keeps "no showings
// scheduled" distinguishable from "no such movie".
func (s *Service) Screenings(ctx context.Context, movieID string) ([]domainmovie.Screening, error) {
	if _, err := s.movies.Get(ctx, movieID); err != nil {
		return nil, err // ErrMovieNotFound or infra
	}
	list, err := s.screenings.ListByMovie(ctx, movieID)
	if err != nil {
		return nil, fmt.Errorf("catalog: list screenings: %w", err)
	}
	return list, nil
}
