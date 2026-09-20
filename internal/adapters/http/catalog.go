package http

import (
	"net/http"
	"time"

	appcatalog "github.com/nathanfabio/bookingConcurrent/internal/application/catalog"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
)

// Catalog DTOs. snake_case enforced by the tag test in booking_test.go.
type (
	movieResponse struct {
		ID              string    `json:"id"`
		Title           string    `json:"title"`
		Synopsis        string    `json:"synopsis"`
		DurationMinutes int       `json:"duration_minutes"`
		CreatedAt       time.Time `json:"created_at"`
	}

	screeningResponse struct {
		ID          string    `json:"id"`
		MovieID     string    `json:"movie_id"`
		StartsAt    time.Time `json:"starts_at"`
		Rows        []string  `json:"rows"`
		SeatsPerRow int       `json:"seats_per_row"`
		// PriceCents is what a payment intent for this screening will freeze
		// (M5, ADR 0008): clients see the price BEFORE holding, and the
		// amount on the intent response must match this exactly.
		PriceCents int       `json:"price_cents"`
		CreatedAt  time.Time `json:"created_at"`
	}

	movieListResponse struct {
		Movies []movieResponse `json:"movies"`
	}

	screeningListResponse struct {
		Screenings []screeningResponse `json:"screenings"`
	}
)

func movieDTO(m domainmovie.Movie) movieResponse {
	return movieResponse{
		ID:              m.ID,
		Title:           m.Title,
		Synopsis:        m.Synopsis,
		DurationMinutes: m.DurationMinutes,
		CreatedAt:       m.CreatedAt,
	}
}

func screeningDTO(s domainmovie.Screening) screeningResponse {
	return screeningResponse{
		ID:          s.ID,
		MovieID:     s.MovieID,
		StartsAt:    s.StartsAt,
		Rows:        s.Rows,
		SeatsPerRow: s.SeatsPerRow,
		PriceCents:  s.PriceCents,
		CreatedAt:   s.CreatedAt,
	}
}

// ListMoviesHandler implements GET /movies: the catalog, ordered by title.
// Public — browsing never needs auth (CLAUDE.md §5).
func ListMoviesHandler(svc *appcatalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		movies, err := svc.Movies(r.Context())
		if err != nil {
			writeBookingError(w, r, err)
			return
		}
		list := make([]movieResponse, 0, len(movies))
		for _, m := range movies {
			list = append(list, movieDTO(m))
		}
		WriteJSON(w, http.StatusOK, movieListResponse{Movies: list})
	}
}

// ListScreeningsHandler implements GET /movies/{movieID}/screenings: one
// movie's showings in chronological order. Public. An unknown movie is a
// 404, not an empty list (see catalog.Service.Screenings).
func ListScreeningsHandler(svc *appcatalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		screenings, err := svc.Screenings(r.Context(), r.PathValue("movieID"))
		if err != nil {
			writeBookingError(w, r, err)
			return
		}
		list := make([]screeningResponse, 0, len(screenings))
		for _, s := range screenings {
			list = append(list, screeningDTO(s))
		}
		WriteJSON(w, http.StatusOK, screeningListResponse{Screenings: list})
	}
}
