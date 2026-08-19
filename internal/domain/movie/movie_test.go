package movie

import (
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

func testScreening() Screening {
	return Screening{
		ID:          "scr-1",
		MovieID:     "movie-1",
		StartsAt:    time.Now(),
		Rows:        []string{"A", "B", "C"},
		SeatsPerRow: 4,
	}
}

func TestHasSeat(t *testing.T) {
	s := testScreening()

	valid := []booking.Seat{{Row: "A", Number: 1}, {Row: "C", Number: 4}, {Row: "B", Number: 2}}
	for _, seat := range valid {
		if !s.HasSeat(seat) {
			t.Errorf("HasSeat(%v) = false, want true", seat)
		}
	}

	invalid := []booking.Seat{
		{Row: "D", Number: 1},  // row does not exist
		{Row: "A", Number: 0},  // number below range
		{Row: "A", Number: 5},  // number above seats_per_row
		{Row: "", Number: 1},   // empty row
		{Row: "A", Number: -1}, // negative
	}
	for _, seat := range invalid {
		if s.HasSeat(seat) {
			t.Errorf("HasSeat(%v) = true, want false", seat)
		}
	}
}

func TestAllSeatsAndCount(t *testing.T) {
	s := testScreening()

	if got := s.SeatCount(); got != 12 {
		t.Fatalf("SeatCount = %d, want 12", got)
	}
	all := s.AllSeats()
	if len(all) != 12 {
		t.Fatalf("AllSeats returned %d seats, want 12", len(all))
	}
	// Row-major order: first seat A1, last seat C4.
	if all[0] != (booking.Seat{Row: "A", Number: 1}) {
		t.Errorf("first seat = %v, want A1", all[0])
	}
	if all[11] != (booking.Seat{Row: "C", Number: 4}) {
		t.Errorf("last seat = %v, want C4", all[11])
	}
	// Every enumerated seat must satisfy HasSeat — the grid and the
	// validator are two views of one geometry and must never disagree.
	for _, seat := range all {
		if !s.HasSeat(seat) {
			t.Errorf("AllSeats produced %v which HasSeat rejects", seat)
		}
	}
}
