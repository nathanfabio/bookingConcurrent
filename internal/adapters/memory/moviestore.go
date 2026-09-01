package memory

import (
	"context"
	"sort"
	"sync"

	"github.com/nathanfabio/bookingConcurrent/internal/application/catalog"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
)

// Compile-time proof the fake satisfies the port.
var _ catalog.MovieStore = (*MovieStore)(nil)

// MovieStore is the in-memory fake of catalog.MovieStore. Tests Add the
// movies they need, then exercise List / Get.
type MovieStore struct {
	mu     sync.Mutex
	movies map[string]domainmovie.Movie
}

// NewMovieStore builds an empty fake.
func NewMovieStore() *MovieStore {
	return &MovieStore{movies: make(map[string]domainmovie.Movie)}
}

// Add registers a movie fixture.
func (s *MovieStore) Add(m domainmovie.Movie) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.movies[m.ID] = m
}

// List implements catalog.MovieStore, ordered by title the way the Postgres
// query is.
func (s *MovieStore) List(ctx context.Context) ([]domainmovie.Movie, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	list := make([]domainmovie.Movie, 0, len(s.movies))
	for _, m := range s.movies {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Title < list[j].Title })
	return list, nil
}

// Get implements catalog.MovieStore.
func (s *MovieStore) Get(ctx context.Context, id string) (domainmovie.Movie, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.movies[id]
	if !ok {
		return domainmovie.Movie{}, domainmovie.ErrMovieNotFound
	}
	return m, nil
}
