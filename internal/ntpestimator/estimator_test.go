package ntpestimator

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEstimator(t *testing.T) {
	e := &Estimator{ClockRate: 90000}

	timeNow = func() time.Time { return time.Date(2003, 11, 4, 23, 15, 7, 0, time.UTC) }
	ntp := e.Estimate(90000)
	require.Equal(t, time.Date(2003, 11, 4, 23, 15, 7, 0, time.UTC), ntp)

	timeNow = func() time.Time { return time.Date(2003, 11, 4, 23, 15, 8, 0, time.UTC) }
	ntp = e.Estimate(2 * 90000)
	require.Equal(t, time.Date(2003, 11, 4, 23, 15, 8, 0, time.UTC), ntp)

	timeNow = func() time.Time { return time.Date(2003, 11, 4, 23, 15, 10, 0, time.UTC) }
	ntp = e.Estimate(3 * 90000)
	require.Equal(t, time.Date(2003, 11, 4, 23, 15, 9, 0, time.UTC), ntp)

	timeNow = func() time.Time { return time.Date(2003, 11, 4, 23, 15, 9, 0, time.UTC) }
	ntp = e.Estimate(4 * 90000)
	require.Equal(t, time.Date(2003, 11, 4, 23, 15, 9, 0, time.UTC), ntp)

	timeNow = func() time.Time { return time.Date(2003, 11, 4, 23, 15, 15, 0, time.UTC) }
	ntp = e.Estimate(5 * 90000)
	require.Equal(t, time.Date(2003, 11, 4, 23, 15, 10, 0, time.UTC), ntp)

	timeNow = func() time.Time { return time.Date(2003, 11, 4, 23, 15, 20, 0, time.UTC) }
	ntp = e.Estimate(6 * 90000)
	require.Equal(t, time.Date(2003, 11, 4, 23, 15, 20, 0, time.UTC), ntp)
}

func TestEstimatorOffset(t *testing.T) {
	prevTimeNow := timeNow
	defer func() { timeNow = prevTimeNow }()

	base := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	timeNow = func() time.Time { return now }

	e := &Estimator{ClockRate: 48000}

	// first estimate is the local clock plus the offset
	e.SetOffset(300 * time.Millisecond)
	ntp := e.Estimate(0)
	require.Equal(t, base.Add(300*time.Millisecond), ntp)

	// the offset stays constant as the track advances
	now = base.Add(time.Second)
	ntp = e.Estimate(48000)
	require.Equal(t, base.Add(time.Second+300*time.Millisecond), ntp)
}

func TestEstimatorOffsetChangeDoesNotGoBackwards(t *testing.T) {
	prevTimeNow := timeNow
	defer func() { timeNow = prevTimeNow }()

	base := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	timeNow = func() time.Time { return now }

	e := &Estimator{ClockRate: 48000}

	// before the first sender report, there is no offset
	ntp := e.Estimate(0)
	require.Equal(t, base, ntp)

	now = base.Add(2 * time.Second)
	ntp = e.Estimate(2 * 48000)
	require.Equal(t, base.Add(2*time.Second), ntp)

	// a sender report arrives and requires shifting the track backwards.
	// The reference is re-anchored and the value is clamped so that the
	// timeline never moves backwards
	e.SetOffset(-500 * time.Millisecond)
	ntp = e.Estimate(2 * 48000)
	require.Equal(t, base.Add(2*time.Second), ntp)

	// afterwards, the timeline keeps advancing
	now = base.Add(3 * time.Second)
	ntp = e.Estimate(3 * 48000)
	require.Equal(t, base.Add(3*time.Second-500*time.Millisecond), ntp)
}
