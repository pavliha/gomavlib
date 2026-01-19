package frame

import "time"

// Clock provides time operations. This interface allows for testing
// with deterministic timestamps.
type Clock interface {
	Now() time.Time
}

// realClock implements Clock using the standard time package.
type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now()
}

// DefaultClock returns the default clock that uses real time.
func DefaultClock() Clock {
	return realClock{}
}
