package recordwindow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	for _, ca := range []struct {
		name    string
		in      string
		wantNil bool
		start   time.Duration
		end     time.Duration
		str     string
	}{
		{name: "empty", in: "", wantNil: true},
		{name: "blank", in: "   ", wantNil: true},
		{
			name:  "same day",
			in:    "10:00-11:30",
			start: 10 * time.Hour,
			end:   11*time.Hour + 30*time.Minute,
			str:   "10:00-11:30",
		},
		{
			name:  "cross midnight",
			in:    "22:15-06:45",
			start: 22*time.Hour + 15*time.Minute,
			end:   6*time.Hour + 45*time.Minute,
			str:   "22:15-06:45",
		},
		{
			name:  "with seconds",
			in:    "23:59:30-00:00:10",
			start: 23*time.Hour + 59*time.Minute + 30*time.Second,
			end:   10 * time.Second,
			str:   "23:59:30-00:00:10",
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			w, err := Parse(ca.in)
			require.NoError(t, err)
			if ca.wantNil {
				require.Nil(t, w)
				return
			}
			require.NotNil(t, w)
			require.Equal(t, ca.start, w.Start)
			require.Equal(t, ca.end, w.End)
			require.Equal(t, ca.str, w.String())
		})
	}
}

func TestParseInvalid(t *testing.T) {
	for _, in := range []string{
		"10:00",
		"10:00-11:00-12:00",
		"abc",
		"25:00-26:00",
		"10:60-11:00",
		"10:00-11:60",
		"10-11",
		"10:000-11:00",
		"10:00-11:00:00x",
		"10:00-10:00", // equal boundaries are ambiguous and rejected
	} {
		t.Run(in, func(t *testing.T) {
			w, err := Parse(in)
			require.Error(t, err, "expected an error for %q", in)
			require.Nil(t, w)
		})
	}
}

func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

func TestContainsSameDay(t *testing.T) {
	w, err := Parse("10:00-11:00")
	require.NoError(t, err)

	require.False(t, w.Contains(utc(2026, time.November, 25, 9, 59)))
	require.True(t, w.Contains(utc(2026, time.November, 25, 10, 0))) // start inclusive
	require.True(t, w.Contains(utc(2026, time.November, 25, 10, 59)))
	require.False(t, w.Contains(utc(2026, time.November, 25, 11, 0))) // end exclusive
	require.False(t, w.Contains(utc(2026, time.November, 25, 15, 0)))

	// the day does not matter
	require.True(t, w.Contains(utc(2030, time.January, 1, 10, 30)))
}

func TestContainsCrossMidnight(t *testing.T) {
	w, err := Parse("23:30-00:30")
	require.NoError(t, err)

	require.False(t, w.Contains(utc(2026, time.November, 25, 23, 0)))
	require.True(t, w.Contains(utc(2026, time.November, 25, 23, 30)))
	require.True(t, w.Contains(utc(2026, time.November, 25, 23, 59)))
	require.True(t, w.Contains(utc(2026, time.November, 26, 0, 0)))
	require.True(t, w.Contains(utc(2026, time.November, 26, 0, 29)))
	require.False(t, w.Contains(utc(2026, time.November, 26, 0, 30))) // end exclusive
	require.False(t, w.Contains(utc(2026, time.November, 26, 12, 0)))
}

func TestNextTransitionSameDay(t *testing.T) {
	w, err := Parse("10:00-11:00")
	require.NoError(t, err)

	for _, ca := range []struct {
		name string
		from time.Time
		want time.Time
	}{
		{
			name: "before start",
			from: utc(2026, time.November, 25, 9, 30),
			want: utc(2026, time.November, 25, 10, 0),
		},
		{
			name: "at start moves to end",
			from: utc(2026, time.November, 25, 10, 0),
			want: utc(2026, time.November, 25, 11, 0),
		},
		{
			name: "inside",
			from: utc(2026, time.November, 25, 10, 30),
			want: utc(2026, time.November, 25, 11, 0),
		},
		{
			name: "at end moves to next start",
			from: utc(2026, time.November, 25, 11, 0),
			want: utc(2026, time.November, 26, 10, 0),
		},
		{
			name: "after end",
			from: utc(2026, time.November, 25, 15, 0),
			want: utc(2026, time.November, 26, 10, 0),
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			require.Equal(t, ca.want, w.NextTransition(ca.from))
		})
	}
}

func TestNextTransitionCrossMidnight(t *testing.T) {
	w, err := Parse("23:30-00:30")
	require.NoError(t, err)

	for _, ca := range []struct {
		name string
		from time.Time
		want time.Time
	}{
		{
			name: "before start",
			from: utc(2026, time.November, 25, 23, 20),
			want: utc(2026, time.November, 25, 23, 30),
		},
		{
			name: "at start moves to end after midnight",
			from: utc(2026, time.November, 25, 23, 30),
			want: utc(2026, time.November, 26, 0, 30),
		},
		{
			name: "before midnight",
			from: utc(2026, time.November, 25, 23, 55),
			want: utc(2026, time.November, 26, 0, 30),
		},
		{
			name: "after midnight",
			from: utc(2026, time.November, 26, 0, 5),
			want: utc(2026, time.November, 26, 0, 30),
		},
		{
			name: "at end moves to next start",
			from: utc(2026, time.November, 26, 0, 30),
			want: utc(2026, time.November, 26, 23, 30),
		},
		{
			name: "daytime",
			from: utc(2026, time.November, 26, 12, 0),
			want: utc(2026, time.November, 26, 23, 30),
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			require.Equal(t, ca.want, w.NextTransition(ca.from))
		})
	}
}
