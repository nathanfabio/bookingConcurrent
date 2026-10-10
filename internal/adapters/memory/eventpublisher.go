package memory

import (
	"context"
	"errors"
	"sync"

	appoutbox "github.com/nathanfabio/bookingConcurrent/internal/application/outbox"
)

// Compile-time proof the fake satisfies the port.
var _ appoutbox.EventPublisher = (*EventPublisher)(nil)

// ErrPublishFailed is the default error FailNext/FailAll inject. Tests may
// pass their own; this one exists so callers don't have to invent an error
// for the common "the broker is down" case.
var ErrPublishFailed = errors.New("memory: publish failed (injected)")

// EventPublisher is the in-memory fake of the outbox.EventPublisher port.
//
// Faithfulness notes: the real adapter's contract is "nil only after the
// broker confirmed", and its realistic failure mode is a dead/unreachable
// broker mid-batch. The fake models exactly that: Publish records the event
// (a "confirm") unless a failure was injected, and injected failures leave
// NOTHING recorded — mirroring a publish that never reached the broker.
// Duplicate publishes ARE recorded twice on purpose: the at-least-once
// tests assert on seeing the same event id published again.
type EventPublisher struct {
	mu       sync.Mutex
	publish  []appoutbox.Event
	failN    int              // fail this many upcoming publishes (consumed)
	failAll  bool             // fail every publish until Reset
	failOn   map[string]error // fail publishes of these event ids until Reset
	injected error
}

// NewEventPublisher builds a healthy fake.
func NewEventPublisher() *EventPublisher {
	return &EventPublisher{}
}

// FailNext makes the next n Publish calls fail with err (or
// ErrPublishFailed when err is nil), recording nothing.
func (p *EventPublisher) FailNext(n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failN = n
	if err == nil {
		err = ErrPublishFailed
	}
	// Reuse failAll's slot semantics: the injected error is stored once.
	p.injected = err
}

// FailAll makes every Publish fail until Reset.
func (p *EventPublisher) FailAll(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failAll = true
	if err == nil {
		err = ErrPublishFailed
	}
	p.injected = err
}

// FailOn makes Publish fail with err whenever the event's ID equals id
// (until Reset) — the lever for mid-batch failures ("the second event
// poisons, the first must still be marked"). A nil err uses ErrPublishFailed.
func (p *EventPublisher) FailOn(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failOn == nil {
		p.failOn = make(map[string]error)
	}
	if err == nil {
		err = ErrPublishFailed
	}
	p.failOn[id] = err
}

// Reset clears injected failures. Recorded publishes are kept.
func (p *EventPublisher) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failN = 0
	p.failAll = false
	p.failOn = nil
}

// Published returns a snapshot of everything published so far, in order,
// duplicates included.
func (p *EventPublisher) Published() []appoutbox.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]appoutbox.Event, len(p.publish))
	copy(out, p.publish)
	return out
}

// PublishedIDs returns the ids of everything published so far, in order.
func (p *EventPublisher) PublishedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.publish))
	for _, e := range p.publish {
		ids = append(ids, e.ID)
	}
	return ids
}

// Publish implements outbox.EventPublisher.
func (p *EventPublisher) Publish(ctx context.Context, evt appoutbox.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err, ok := p.failOn[evt.ID]; ok {
		return err
	}
	if p.failAll {
		return p.injected
	}
	if p.failN > 0 {
		p.failN--
		return p.injected
	}
	p.publish = append(p.publish, evt)
	return nil
}
