package ratelimit

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeClock is a manually controlled clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

const (
	testRate   = 1000        // bytes per second
	testWindow = time.Second // 1000 bytes budget
)

func TestLimiterDisabledByDefault(t *testing.T) {
	clk := newFakeClock()
	l := New(0, testWindow, clk.Now)

	// Any amount is accepted when the rate is zero.
	for range 100 {
		require.False(t, l.Add(1<<20))
	}
	require.False(t, l.Exceeded())
}

func TestLimiterThresholdBoundary(t *testing.T) {
	clk := newFakeClock()
	l := New(testRate, testWindow, clk.Now)

	// Exactly rate*window bytes are allowed (limit is strictly greater).
	require.False(t, l.Add(1000))
	require.False(t, l.Exceeded())

	// A single extra byte crosses the threshold.
	require.True(t, l.Add(1))
	require.True(t, l.Exceeded())
}

func TestLimiterShortBurstInsideBudgetIsAllowed(t *testing.T) {
	clk := newFakeClock()
	l := New(testRate, 2*time.Second, clk.Now) // 2000 bytes budget

	// A short 2000-byte burst is absorbed by the window.
	for range 20 {
		require.False(t, l.Add(100))
	}
	require.False(t, l.Exceeded())

	// Sustaining the burst immediately goes over.
	require.True(t, l.Add(1))
}

func TestLimiterSustainedHighRateTrips(t *testing.T) {
	clk := newFakeClock()
	// 1000 B/s limit, 1 s window. Feed 200 B every 100 ms = 2000 B/s.
	l := New(testRate, testWindow, clk.Now)

	trippedAt := -1
	for i := range 30 {
		exceeded := l.Add(200)
		clk.Advance(100 * time.Millisecond)
		if exceeded {
			trippedAt = i
			break
		}
	}

	// Budget is 1000 bytes; after five 200 B samples (1000 B) it is still
	// allowed, the sixth (1200 B) trips.
	require.Equal(t, 5, trippedAt)
	require.True(t, l.Exceeded())

	// It stays exceeded while the high rate is sustained.
	for range 10 {
		require.True(t, l.Add(200))
		clk.Advance(100 * time.Millisecond)
	}
}

func TestLimiterLowRateNeverTrips(t *testing.T) {
	clk := newFakeClock()
	l := New(testRate, testWindow, clk.Now)

	// 50 B every 100 ms = 500 B/s, run for 30 s of fake time.
	for range 300 {
		require.False(t, l.Add(50))
		require.False(t, l.Exceeded())
		clk.Advance(100 * time.Millisecond)
	}
}

func TestLimiterRecoversAfterWindowSlides(t *testing.T) {
	clk := newFakeClock()
	l := New(testRate, testWindow, clk.Now)

	require.True(t, l.Add(1500))
	require.True(t, l.Exceeded())

	// Advancing less than the window keeps the bytes.
	clk.Advance(500 * time.Millisecond)
	require.True(t, l.Exceeded())

	// Once the oversized sample is older than the window, it is evicted and
	// the limiter recovers.
	clk.Advance(600 * time.Millisecond)
	require.False(t, l.Exceeded())

	// New traffic is accepted again.
	require.False(t, l.Add(100))
}

func TestLimiterSamplesSlideOutGradually(t *testing.T) {
	clk := newFakeClock()
	l := New(testRate, testWindow, clk.Now)

	// 200 B every 200 ms = 1000 B/s exactly: never strictly over.
	for range 20 {
		require.False(t, l.Add(200))
		clk.Advance(200 * time.Millisecond)
	}
	require.False(t, l.Exceeded())
}

func TestLimiterClockRollbackResetsWindow(t *testing.T) {
	clk := newFakeClock()
	l := New(testRate, testWindow, clk.Now)

	require.True(t, l.Add(5000))
	require.True(t, l.Exceeded())

	// Clock moves backwards far behind the samples. Instead of a negative
	// window or a panic, history is dropped and the limiter is healthy again.
	clk.Set(clk.Now().Add(-1 * time.Hour))
	require.False(t, l.Exceeded())
	require.False(t, l.Add(10))
	require.False(t, l.Exceeded())

	// Time then advances normally and accounting keeps working.
	clk.Advance(time.Second)
	require.False(t, l.Exceeded())
}

func TestLimiterResetOnPublisherSwitch(t *testing.T) {
	clk := newFakeClock()
	l := New(testRate, testWindow, clk.Now)

	require.True(t, l.Add(5000))
	require.True(t, l.Exceeded())

	// A replacement publisher starts with a fresh window, it must not be
	// charged for the previous publisher bytes.
	l.Reset()
	require.False(t, l.Exceeded())
	require.False(t, l.Add(100))
	require.False(t, l.Exceeded())
}

func TestLimiterConfigureHotReload(t *testing.T) {
	clk := newFakeClock()
	l := New(testRate, testWindow, clk.Now)

	// 1500 B/s sustained for a while trips the 1000 B/s limit.
	for range 8 {
		l.Add(150)
		clk.Advance(100 * time.Millisecond)
	}
	require.True(t, l.Exceeded())

	// Hot reload raising the limit above the current rate relieves it once
	// eviction runs with the new allowance.
	l.Configure(10000, testWindow)
	require.False(t, l.Exceeded())

	// Hot reload lowering the limit below the bytes currently held trips it
	// immediately on the next observation.
	l.Configure(100, testWindow)
	require.True(t, l.Exceeded())

	// Hot reload disabling the limit clears the state.
	l.Configure(0, testWindow)
	require.False(t, l.Exceeded())
	require.False(t, l.Add(1<<30))
}

func TestLimiterConfigureShorterWindowEvictsImmediately(t *testing.T) {
	clk := newFakeClock()
	l := New(1000, 10*time.Second, clk.Now)

	l.Add(5000)
	require.False(t, l.Exceeded()) // budget is 10000

	clk.Advance(2 * time.Second)
	l.Add(0)

	// Reload with a 1 s window: the 5000 B sample is now old and evicted.
	l.Configure(1000, time.Second)
	require.False(t, l.Exceeded())
}
