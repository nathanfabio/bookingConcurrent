package outbox

import (
	"context"
	"fmt"
)

// Delivery is a point-in-time view of an event and its notification claim.
type Delivery struct {
	Event     Event
	Published bool
	Processed bool
}

// Republish handles a single operator decision. A claim may arrive after
// the read, so even a deliberate replay must carry the original event ID.
func (d Delivery) Republish(ctx context.Context, publisher EventPublisher) (string, error) {
	if d.Processed {
		return "already_processed", nil
	}
	if !d.Published {
		return "", fmt.Errorf("event %s is pending normal publication", d.Event.ID)
	}
	if d.Event.Type != "BookingConfirmed" {
		return "", fmt.Errorf("unsupported recovery event type %q", d.Event.Type)
	}
	if err := publisher.Publish(ctx, d.Event); err != nil {
		return "", fmt.Errorf("republish %s: %w", d.Event.ID, err)
	}
	return "republished", nil
}
