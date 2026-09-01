package booking

import (
	"context"

	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
)

// ScreeningStore is the port the booking use cases use to load screening
// geometry — the seat grid that hold validation and the seat map are built
// from (CLAUDE.md §4). The Postgres ScreeningRepo satisfies it; the
// in-memory fake serves unit tests.
//
// Only the read the use cases actually need lives here (Get). Listing
// screenings for clients is the catalog's job, not the booking flow's —
// the interface stays as small as its consumer (ADR 0001).
type ScreeningStore interface {
	// Get loads one screening by id.
	//
	// Errors: domainmovie.ErrScreeningNotFound.
	Get(ctx context.Context, id string) (domainmovie.Screening, error)
}
