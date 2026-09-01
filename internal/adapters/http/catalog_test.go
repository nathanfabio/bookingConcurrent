package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
)

func TestListMoviesHandler(t *testing.T) {
	env := newBookingEnv(4)
	env.movies.Add(domainmovie.Movie{
		ID: "movie-b", Title: "Beta Film", Synopsis: "second", DurationMinutes: 90,
		CreatedAt: bookingTestStart,
	})
	env.movies.Add(domainmovie.Movie{
		ID: "movie-a", Title: "Alpha Film", Synopsis: "first", DurationMinutes: 120,
		CreatedAt: bookingTestStart,
	})

	rec := httptest.NewRecorder()
	// Public endpoint: no authenticated user.
	ListMoviesHandler(env.catalog).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/movies", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var body movieListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(body.Movies) != 2 {
		t.Fatalf("movie count = %d, want 2", len(body.Movies))
	}
	// Ordered by title, not insertion order.
	if body.Movies[0].Title != "Alpha Film" || body.Movies[1].Title != "Beta Film" {
		t.Errorf("movies out of title order: %q, %q", body.Movies[0].Title, body.Movies[1].Title)
	}
	if body.Movies[0].ID != "movie-a" || body.Movies[0].DurationMinutes != 120 {
		t.Errorf("movie DTO fields wrong: %+v", body.Movies[0])
	}

	// snake_case on the wire.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["movies"]; !ok {
		t.Errorf("response missing snake_case field \"movies\"; got %v", raw)
	}
}

func TestListScreeningsHandler(t *testing.T) {
	env := newBookingEnv(4)
	env.movies.Add(domainmovie.Movie{ID: testMovie, Title: "M", DurationMinutes: 100})
	// Insert out of order; the handler must return chronological order.
	env.screenings.Add(domainmovie.Screening{
		ID: "sc-late", MovieID: testMovie, StartsAt: bookingTestStart.Add(48 * time.Hour),
		Rows: []string{"A"}, SeatsPerRow: 2,
	})
	env.screenings.Add(domainmovie.Screening{
		ID: "sc-early", MovieID: testMovie, StartsAt: bookingTestStart.Add(24 * time.Hour),
		Rows: []string{"A"}, SeatsPerRow: 2,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/movies/"+testMovie+"/screenings", nil)
	req.SetPathValue("movieID", testMovie)
	ListScreeningsHandler(env.catalog).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var body screeningListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(body.Screenings) != 2 {
		t.Fatalf("screening count = %d, want 2", len(body.Screenings))
	}
	if body.Screenings[0].ID != "sc-early" || body.Screenings[1].ID != "sc-late" {
		t.Errorf("screenings out of order: %q, %q", body.Screenings[0].ID, body.Screenings[1].ID)
	}
}

func TestListScreeningsUnknownMovie(t *testing.T) {
	env := newBookingEnv(4)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/movies/no-such-movie/screenings", nil)
	req.SetPathValue("movieID", "no-such-movie")
	ListScreeningsHandler(env.catalog).ServeHTTP(rec, req)
	// Unknown movie is a 404, not an empty list.
	assertErrorResponse(t, rec, http.StatusNotFound, CodeNotFound)
}
