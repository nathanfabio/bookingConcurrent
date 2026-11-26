//go:build e2e

package e2e

import (
	"slices"
	"testing"
)

func TestRestoredBooking(t *testing.T) {
	f := loadCheckout(t)
	owner := login(t, "owner@example.test")
	var got confirmedBooking
	request(t, "POST", "/holds/"+f.Booking.Session+"/confirm", owner, nil, 200, &got)
	if got != f.Booking {
		t.Fatalf("restored booking = %+v, want %+v", got, f.Booking)
	}
	var grid struct{ Seats []seat }
	request(t, "GET", "/screenings/"+f.Booking.Screening+"/seats", "", nil, 200, &grid)
	sold := seat{Row: got.Row, Number: got.Number, Status: "booked"}
	if !slices.Contains(grid.Seats, sold) {
		t.Fatalf("restored seat map missing %+v", sold)
	}
}
