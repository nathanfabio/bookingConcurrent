package refund

import (
	"context"
	"errors"
	"testing"
)

type testStore struct {
	pending   []Request
	completed map[string]string
}

func (s *testStore) NextBatch(context.Context, int) ([]Request, error) { return s.pending, nil }
func (s *testStore) Complete(_ context.Context, id, ref string) error {
	s.completed[id] = ref
	return nil
}

type testGateway struct{ failure error }

func (g testGateway) Refund(_ context.Context, r Request) (string, error) {
	if r.PaymentID == "first" {
		return "", g.failure
	}
	return "receipt", nil
}

func TestFailedSubmissionLeavesOtherRequestsRunnable(t *testing.T) {
	s := testStore{pending: []Request{{PaymentID: "first"}, {PaymentID: "second"}}, completed: make(map[string]string)}
	failure := errors.New("provider unavailable")
	p := Processor{Store: &s, Gateway: testGateway{failure}, BatchSize: 2}
	n, err := p.RunOnce(t.Context())
	if n != 1 || !errors.Is(err, failure) || len(s.completed) != 1 || s.completed["second"] != "receipt" {
		t.Fatalf("completed=%v n=%d err=%v", s.completed, n, err)
	}
}
