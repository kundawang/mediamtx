package recorder

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeTimer implements Timer with a manually driven deadline.
type fakeTimer struct {
	c       chan time.Time
	mutex   sync.Mutex
	fired   bool
	stopped bool
}

func (t *fakeTimer) C() <-chan time.Time {
	return t.c
}

func (t *fakeTimer) Stop() bool {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	wasActive := !t.fired && !t.stopped
	t.stopped = true
	return wasActive
}

// fakeClock is a manually driven Clock. Time only moves when Advance is
// called, which fires every timer whose deadline has been reached.
type fakeClock struct {
	mutex  sync.Mutex
	now    time.Time
	timers map[*fakeTimer]time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{
		now:    start,
		timers: make(map[*fakeTimer]time.Time),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// A generous buffer means firing never blocks on a timer whose event has
	// not been consumed yet.
	t := &fakeTimer{c: make(chan time.Time, 64)}
	if d <= 0 {
		d = 1
	}
	c.timers[t] = c.now.Add(d)
	return t
}

// activeTimerCount returns the number of timers that have neither fired nor
// been stopped. It is used by tests to synchronize with the recorder goroutine.
func (c *fakeClock) activeTimerCount() int {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	n := 0
	for ft := range c.timers {
		ft.mutex.Lock()
		if !ft.fired && !ft.stopped {
			n++
		}
		ft.mutex.Unlock()
	}
	return n
}

// waitTimers waits until n active timers are registered.
func waitTimers(t *testing.T, clk *fakeClock, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return clk.activeTimerCount() == n
	}, 3*time.Second, time.Millisecond, "expected %d active clock timers, got %d", n, clk.activeTimerCount())
}

// Advance moves the clock forward and fires, in deadline order, every timer
// whose deadline has been reached.
func (c *fakeClock) Advance(d time.Duration) {
	c.mutex.Lock()
	c.now = c.now.Add(d)

	var due []*fakeTimer

	for {
		var next *fakeTimer
		var nextDeadline time.Time

		for t, deadline := range c.timers {
			t.mutex.Lock()
			inactive := t.fired || t.stopped
			t.mutex.Unlock()
			if inactive {
				continue
			}
			if !deadline.After(c.now) && (next == nil || deadline.Before(nextDeadline)) {
				next = t
				nextDeadline = deadline
			}
		}

		if next == nil {
			break
		}

		next.mutex.Lock()
		next.fired = true
		next.mutex.Unlock()
		due = append(due, next)
	}

	now := c.now
	c.mutex.Unlock()

	// Notify after releasing the clock lock so a slow consumer can never
	// deadlock Advance.
	for _, t := range due {
		select {
		case t.c <- now:
		default:
		}
	}
}
