package recorder

import "time"

// Timer is the subset of *time.Timer used by the recorder.
// It is an interface so time can be controlled in tests.
type Timer interface {
	// C returns the channel on which the timer fires.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It returns true if the call
	// stops the timer before it fires.
	Stop() bool
}

// Clock is the source of current time and timers used by the recorder.
// The production implementation is systemClock; tests can replace it with
// a manually driven clock.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// NewTimer creates a timer that fires after d.
	NewTimer(d time.Duration) Timer
}

type systemTimer struct {
	t *time.Timer
}

func (t *systemTimer) C() <-chan time.Time {
	return t.t.C
}

func (t *systemTimer) Stop() bool {
	return t.t.Stop()
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

func (systemClock) NewTimer(d time.Duration) Timer {
	return &systemTimer{t: time.NewTimer(d)}
}
