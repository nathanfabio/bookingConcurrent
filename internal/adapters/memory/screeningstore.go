package memory

import (
	"context"
	"sort"
	"sync"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	"github.com/nathanfabio/bookingConcurrent/internal/application/catalog"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
)

// Compile-time proof the fake satisfies both ports it serves. The Postgres
// ScreeningRepo likewise backs the booking geometry read and the catalog
// listing, so one fake covering both mirrors the real adapter.
var (
	_ appbooking.ScreeningStore = (*ScreeningStore)(nil)
	_ catalog.ScreeningLister   = (*ScreeningStore)(nil)
)

// ScreeningStore is the in-memory fake of the screening read ports. Tests
// Add the screenings they need, then exercise Get / ListByMovie.
type ScreeningStore struct {
	mu         sync.Mutex
	screenings map[string]domainmovie.Screening
}

// NewScreeningStore builds an empty fake.
func NewScreeningStore() *ScreeningStore {
	return &ScreeningStore{screenings: make(map[string]domainmovie.Screening)}
}

// Add registers a screening fixture.
func (s *ScreeningStore) Add(sc domainmovie.Screening) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.screenings[sc.ID] = sc
}

// Get implements booking.ScreeningStore.
func (s *ScreeningStore) Get(ctx context.Context, id string) (domainmovie.Screening, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sc, ok := s.screenings[id]
	if !ok {
		return domainmovie.Screening{}, domainmovie.ErrScreeningNotFound
	}
	return sc, nil
}

// ListByMovie implements catalog.ScreeningLister, ordered by start time the
// way the Postgres query is.
func (s *ScreeningStore) ListByMovie(ctx context.Context, movieID string) ([]domainmovie.Screening, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	list := make([]domainmovie.Screening, 0)
	for _, sc := range s.screenings {
		if sc.MovieID == movieID {
			list = append(list, sc)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].StartsAt.Before(list[j].StartsAt) })
	return list, nil
}
