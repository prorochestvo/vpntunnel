package tunnelpool

import "time"

// Clock abstracts time operations so tests can inject a fake without real
// sleeps. Both the streaming supervisor (Task 3) and the on-demand scheduler
// (Task 4) use this interface.
//
// After returns a channel that fires once after duration d. The caller must
// drain or discard the channel when it is no longer needed, as with time.After.
// Now returns the current time, used for handshake-age comparisons.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// NewRealClock returns a Clock backed by the standard library.
func NewRealClock() Clock { return realClock{} }

// realClock is the production Clock implementation.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
