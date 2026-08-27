// Package memory provides in-memory fakes of the application ports for
// unit tests (CLAUDE.md §9: every port gets a fake exercised by unit
// tests).
//
// These are fakes, not mocks: they implement the port's real semantics
// (atomicity via a mutex, TTL expiry via an injectable clock) rather than
// scripting per-call expectations. The injectable clock is what makes TTL
// behavior testable deterministically — a test advances time instead of
// sleeping.
package memory

import (
	"sync"
	"time"
)

// Clock abstracts time so tests can advance it deterministically.
type Clock interface {
	Now() time.Time
}

// RealClock is the wall-clock implementation for non-test use (e.g. the
// sweeper's idle tick in dev mode, should anyone wire it).
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// ManualClock is a test clock. It is safe for concurrent use.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock starts the clock at start.
func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{now: start}
}

// Now returns the clock's current (manual) time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d. Concurrent calls are safe.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to an absolute instant. Tests use it to recover
// after advancing past an expiry they wanted to observe. Concurrent calls
// are safe.
func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
