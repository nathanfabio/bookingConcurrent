package memory

import (
	"context"
	"errors"
	"sync"

	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"
)

// Compile-time proof the fake satisfies the port.
var _ appnotification.Notifier = (*Notifier)(nil)

// ErrNotifyFailed is the default error FailNext injects.
var ErrNotifyFailed = errors.New("memory: notify failed (injected)")

// Notifier is the in-memory fake of the notification.Notifier port: it
// RECORDS every notification instead of sending it, so tests assert on the
// side effect's count and content.
//
// FailNext injects one-shot failures — the lever for pinning the handler's
// at-most-once consequence (notify fails after the claim ⇒ the redelivery
// is skipped as a duplicate and the notifier is never called again).
type Notifier struct {
	mu       sync.Mutex
	calls    []appnotification.BookingConfirmed
	attempts int // every invocation, failures included
	failN    int
	injected error
}

// NewNotifier builds a healthy fake.
func NewNotifier() *Notifier {
	return &Notifier{}
}

// FailNext makes the next n NotifyBookingConfirmed calls fail with err (or
// ErrNotifyFailed when nil), recording nothing.
func (n *Notifier) FailNext(count int, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failN = count
	if err == nil {
		err = ErrNotifyFailed
	}
	n.injected = err
}

// Calls returns a snapshot of every recorded notification, in order.
func (n *Notifier) Calls() []appnotification.BookingConfirmed {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]appnotification.BookingConfirmed, len(n.calls))
	copy(out, n.calls)
	return out
}

// CallCount returns how many notifications were recorded.
func (n *Notifier) CallCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

// Attempts returns how many times NotifyBookingConfirmed was invoked,
// INCLUDING failed attempts — the assertion behind the handler's
// at-most-once consequence (after a notify failure, a redelivery must not
// re-invoke the notifier at all).
func (n *Notifier) Attempts() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.attempts
}

// NotifyBookingConfirmed implements notification.Notifier.
func (n *Notifier) NotifyBookingConfirmed(ctx context.Context, evt appnotification.BookingConfirmed) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.attempts++
	if n.failN > 0 {
		n.failN--
		return n.injected
	}
	n.calls = append(n.calls, evt)
	return nil
}
