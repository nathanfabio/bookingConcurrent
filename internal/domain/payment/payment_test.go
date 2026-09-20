package payment_test

import (
	"testing"

	"github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
)

// allStatuses is the exhaustive status set — the same values migration
// 00005's CHECK constraint allows. The transition-table test iterates the
// full 6×6 product so a new status cannot be added without the test
// demanding an explicit expectation for every edge it touches.
var allStatuses = []payment.Status{
	payment.StatusIntent,
	payment.StatusAuthorized,
	payment.StatusCaptured,
	payment.StatusRefunding,
	payment.StatusRefunded,
	payment.StatusFailed,
}

func TestStatusValidKnownAndUnknown(t *testing.T) {
	for _, s := range allStatuses {
		if !s.Valid() {
			t.Errorf("Status(%q).Valid() = false, want true", s)
		}
	}
	for _, s := range []payment.Status{"", "INTENT", "chargeback", "pending"} {
		if s.Valid() {
			t.Errorf("Status(%q).Valid() = true, want false", s)
		}
	}
}

func TestStatusTransitionTable(t *testing.T) {
	// wanted[from] = the set of legal next statuses. Everything else — the
	// complement across the 6×6 product — must be false. The load-bearing
	// edges: captured exits ONLY to refunding (money that moved can never
	// "become failed"), failed and refunded are terminal, and same-status
	// is never a transition (idempotent replays are store-level no-ops, not
	// domain edges).
	wanted := map[payment.Status]map[payment.Status]bool{
		payment.StatusIntent: {
			payment.StatusAuthorized: true,
			payment.StatusCaptured:   true,
			payment.StatusFailed:     true,
		},
		payment.StatusAuthorized: {
			payment.StatusCaptured: true,
			payment.StatusFailed:   true,
		},
		payment.StatusCaptured: {
			payment.StatusRefunding: true,
		},
		payment.StatusRefunding: {
			payment.StatusRefunded: true,
		},
		payment.StatusRefunded: {}, // terminal
		payment.StatusFailed:   {}, // terminal
	}

	for _, from := range allStatuses {
		for _, to := range allStatuses {
			want := wanted[from][to]
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("(%s).CanTransitionTo(%s) = %v, want %v", from, to, got, want)
			}
		}
	}

	// An unknown status transitions nowhere — the default branch is not
	// dead code, it is the failure mode for a row corrupted outside the
	// CHECK constraint's vocabulary.
	if payment.Status("nonsense").CanTransitionTo(payment.StatusCaptured) {
		t.Error("unknown status must not transition anywhere")
	}
}

func TestPaymentIsCaptured(t *testing.T) {
	for _, s := range allStatuses {
		p := payment.Payment{Status: s}
		if got, want := p.IsCaptured(), s == payment.StatusCaptured; got != want {
			t.Errorf("Payment{Status: %s}.IsCaptured() = %v, want %v", s, got, want)
		}
	}
}
