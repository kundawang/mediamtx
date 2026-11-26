// Package ntpestimator contains a NTP estimator.
package ntpestimator

import (
	"sync"
	"time"
)

const (
	maxTimeDifference = 5 * time.Second
)

var timeNow = time.Now

func multiplyAndDivide(v, m, d time.Duration) time.Duration {
	secs := v / d
	dec := v % d
	return (secs*m + dec*m/d)
}

// Estimator is a NTP estimator.
type Estimator struct {
	ClockRate int

	mutex sync.Mutex

	// offset is added to every estimated NTP. It is used to align tracks
	// that share the same capture clock but did not start at the same time.
	offset time.Duration

	refNTP time.Time
	refPTS int64

	// lastEstimated is the last value returned by Estimate, used to make
	// sure that a change of offset never moves the timeline backwards.
	lastEstimated time.Time
}

var zero = time.Time{}

// SetOffset sets a constant offset added to every estimated NTP.
// When the offset changes, the reference is re-anchored so that the estimated
// timeline keeps advancing without a discontinuity.
func (e *Estimator) SetOffset(offset time.Duration) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if offset == e.offset {
		return
	}

	e.offset = offset
	e.refNTP = zero
	e.refPTS = 0
}

// Offset returns the offset added to every estimated NTP.
func (e *Estimator) Offset() time.Duration {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.offset
}

// Estimate returns estimated NTP.
func (e *Estimator) Estimate(pts int64) time.Time {
	now := timeNow()

	// do not store monotonic clock, in order to include
	// system clock changes into time differences
	now = now.Round(0)

	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.refNTP.Equal(zero) {
		e.refNTP = now
		e.refPTS = pts
		return e.store(now.Add(e.offset))
	}

	computed := e.refNTP.Add(multiplyAndDivide(time.Duration(pts-e.refPTS), time.Second, time.Duration(e.ClockRate)))

	if computed.After(now) || computed.Before(now.Add(-maxTimeDifference)) {
		e.refNTP = now
		e.refPTS = pts
		return e.store(now.Add(e.offset))
	}

	return e.store(computed.Add(e.offset))
}

// store records and returns the estimated NTP, clamping it so that estimated
// values never move backwards.
func (e *Estimator) store(estimated time.Time) time.Time {
	if !e.lastEstimated.Equal(zero) && estimated.Before(e.lastEstimated) {
		estimated = e.lastEstimated
	}

	e.lastEstimated = estimated
	return estimated
}
