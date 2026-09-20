// Package movie holds the catalog domain model: movies and their
// screenings, including the seat-grid geometry that holds and the seat map
// are validated against (CLAUDE.md §4).
package movie

import (
	"errors"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

// Domain errors surfaced by the catalog stores. Mapped to HTTP statuses
// centrally (404s) once the handlers land.
var (
	ErrMovieNotFound     = errors.New("movie: not found")
	ErrScreeningNotFound = errors.New("movie: screening not found")
)

// Movie is a film in the catalog. It is reference data: nothing about a
// movie changes because someone books a seat.
type Movie struct {
	ID              string
	Title           string
	Synopsis        string
	DurationMinutes int
	CreatedAt       time.Time
}

// Screening is one bookable showing of a movie. All seat state — holds in
// Redis, confirmed bookings in Postgres — is scoped to a screening.
type Screening struct {
	ID          string
	MovieID     string
	StartsAt    time.Time
	Rows        []string // explicit row labels, e.g. {A, B, C}
	SeatsPerRow int
	// PriceCents is the ticket price for this screening, in cents. It is the
	// pricing source of truth: a payment intent FREEZES it into the payment
	// row at creation (migration 00005 stores amount_cents per payment), so
	// later price changes never rewrite what a buyer was charged. Positive
	// by schema CHECK (migration 00008) — a zero-amount intent is a gateway
	// edge case with no business meaning here.
	PriceCents int
	CreatedAt  time.Time
}

// HasSeat reports whether the screening's geometry contains seat. This is
// the out-of-range validation CLAUDE.md §4 requires before any hold
// touches Redis — the use case calls it first.
func (s Screening) HasSeat(seat booking.Seat) bool {
	if seat.Number < 1 || seat.Number > s.SeatsPerRow {
		return false
	}
	for _, row := range s.Rows {
		if row == seat.Row {
			return true
		}
	}
	return false
}

// AllSeats enumerates every seat in row-major order. The seat-map endpoint
// (GET /screenings/{id}/seats) walks this to render the full grid with
// per-seat status.
func (s Screening) AllSeats() []booking.Seat {
	seats := make([]booking.Seat, 0, len(s.Rows)*s.SeatsPerRow)
	for _, row := range s.Rows {
		for n := 1; n <= s.SeatsPerRow; n++ {
			seats = append(seats, booking.Seat{Row: row, Number: n})
		}
	}
	return seats
}

// SeatCount returns the total number of seats in the screening.
func (s Screening) SeatCount() int {
	return len(s.Rows) * s.SeatsPerRow
}
