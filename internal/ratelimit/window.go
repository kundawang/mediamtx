// Package ratelimit provides rate limiters used to protect paths from
// publishers that sustain an excessive inbound byte rate.
package ratelimit

import (
	"sync"
	"time"
)

// Clock returns the current time. It can be replaced in tests with a
// controllable (fake) clock.
type Clock func() time.Time

func defaultClock() time.Time {
	return time.Now()
}

type sample struct {
	at    time.Time
	bytes int64
}

// Limiter is a sliding-window byte rate limiter.
//
// It records the amount of bytes received at successive points in time and
// keeps only the samples whose age is smaller than the window. The limit is
// exceeded when the sum of the bytes inside the window is strictly greater
// than byteRate * window.
//
// A byteRate of zero disables the limiter: Add always reports that the limit
// has not been exceeded.
//
// The limiter is safe for concurrent use.
type Limiter struct {
	mu sync.Mutex

	byteRate int64
	window   time.Duration
	now      Clock

	samples []sample
	total   int64
	last    time.Time
	hasLast bool
}

// New allocates a Limiter.
// byteRate is the maximum sustained rate, expressed in bytes per second.
// window is the finite sliding time interval over which bytes are summed.
// If now is nil, time.Now is used.
func New(byteRate int64, window time.Duration, now Clock) *Limiter {
	if now == nil {
		now = defaultClock
	}

	l := &Limiter{
		byteRate: byteRate,
		window:   window,
		now:      now,
	}

	return l
}

// Configure changes the byte rate and window of an existing limiter.
// Already recorded samples are preserved and are evicted according to the new
// window on the next Add or Exceeded call. A zero byteRate disables the
// limiter. This makes configuration hot reload deterministic.
func (l *Limiter) Configure(byteRate int64, window time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.byteRate = byteRate
	l.window = window
}

// Reset clears all the recorded samples. It must be called whenever a new
// publisher starts publishing to a path, so bytes received by a previous
// publisher are never charged to the new one.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.samples = nil
	l.total = 0
	l.hasLast = false
	l.last = time.Time{}
}

// ByteRate returns the configured rate in bytes per second.
func (l *Limiter) ByteRate() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byteRate
}

// Window returns the configured sliding window.
func (l *Limiter) Window() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.window
}

// allowance returns the maximum amount of bytes that can be held inside the
// window without exceeding the rate.
func (l *Limiter) allowance() float64 {
	return float64(l.byteRate) * l.window.Seconds()
}

// evict removes samples that are older than the window.
// Lock must be held.
func (l *Limiter) evict(now time.Time) {
	if l.window <= 0 {
		// A non-positive window does not retain any history.
		l.total = 0
		l.samples = l.samples[:0]
		return
	}

	cutoff := now.Add(-l.window)
	keep := 0
	for keep < len(l.samples) && !l.samples[keep].at.After(cutoff) {
		// sample.at <= cutoff, i.e. age >= window: evict.
		l.total -= l.samples[keep].bytes
		keep++
	}

	if keep > 0 {
		n := copy(l.samples, l.samples[keep:])
		for i := n; i < len(l.samples); i++ {
			l.samples[i] = sample{}
		}
		l.samples = l.samples[:n]
	}
}

// add records n bytes and returns whether the limit is currently exceeded.
// n must be non-negative.
func (l *Limiter) add(n int64) bool {
	if n < 0 {
		n = 0
	}

	now := l.now()

	// A wall clock can be moved backwards (NTP, manual change, container
	// suspend/resume). Instead of producing a negative or nonsensical
	// window, drop the history and start a fresh one. This is deterministic
	// and never causes a false violation because of the clock change alone.
	if l.hasLast && now.Before(l.last) {
		l.samples = nil
		l.total = 0
	}

	l.last = now
	l.hasLast = true

	if l.byteRate <= 0 || l.window <= 0 {
		// Disabled: do not retain samples and never trigger.
		l.samples = nil
		l.total = 0
		return false
	}

	l.evict(now)

	l.samples = append(l.samples, sample{at: now, bytes: n})
	l.total += n

	return float64(l.total) > l.allowance()
}

// Add records n received bytes at the current (possibly controlled) time and
// returns true if the sustained rate inside the sliding window is over the
// configured limit.
func (l *Limiter) Add(n int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.add(n)
}

// Exceeded reports whether the rate is currently exceeded, without adding new
// bytes. Time-based eviction is performed against the current clock.
func (l *Limiter) Exceeded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.byteRate <= 0 || l.window <= 0 {
		return false
	}

	now := l.now()

	if l.hasLast && now.Before(l.last) {
		l.samples = nil
		l.total = 0
		l.last = now
		return false
	}

	l.evict(now)

	return float64(l.total) > l.allowance()
}
