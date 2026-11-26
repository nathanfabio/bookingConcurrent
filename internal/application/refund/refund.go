// Package refund executes authorized full refunds against an idempotent provider.
package refund

import (
	"context"
	"errors"
	"fmt"
)

type Request struct {
	PaymentID   string
	IntentID    string
	AmountCents int
	Currency    string
}

type Store interface {
	NextBatch(context.Context, int) ([]Request, error)
	Complete(context.Context, string, string) error
}

type Gateway interface {
	// Refund returns the original receipt when the same payment and terms replay.
	Refund(context.Context, Request) (string, error)
}

type Processor struct {
	Store     Store
	Gateway   Gateway
	BatchSize int
}

func (p Processor) RunOnce(ctx context.Context) (int, error) {
	requests, err := p.Store.NextBatch(ctx, p.BatchSize)
	if err != nil {
		return 0, err
	}
	var failures []error
	completed := 0
	for _, r := range requests {
		if ctx.Err() != nil {
			return completed, errors.Join(append(failures, ctx.Err())...)
		}
		ref, err := p.Gateway.Refund(ctx, r)
		if err == nil {
			err = p.Store.Complete(ctx, r.PaymentID, ref)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("refund %s: %w", r.PaymentID, err))
			continue
		}
		completed++
	}
	return completed, errors.Join(failures...)
}
