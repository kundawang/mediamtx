// Package recordwindow contains the daily UTC recording window.
package recordwindow

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is a daily recurring time interval expressed in UTC.
//
// Start and End are offsets from midnight UTC, in the range [0, 24h).
// When Start < End the window is contained in a single day.
// When Start > End the window crosses midnight (for example 22:00-06:00).
// Start and End are never equal: Parse rejects such intervals, and an empty
// window string (nil *Window) is used to record around the clock.
type Window struct {
	Start time.Duration
	End   time.Duration
}

// Parse parses a daily window in the form "HH:MM-HH:MM" or
// "HH:MM:SS-HH:MM:SS", with both times expressed in UTC.
// An empty string returns a nil window, meaning that no window is configured.
func Parse(in string) (*Window, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return nil, nil
	}

	parts := strings.Split(in, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid record window %q: must be in the form HH:MM-HH:MM (UTC)", in)
	}

	start, err := parseTimeOfDay(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid record window %q: %w", in, err)
	}

	end, err := parseTimeOfDay(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid record window %q: %w", in, err)
	}

	if start == end {
		return nil, fmt.Errorf("invalid record window %q: start and end must differ; "+
			"leave the record window empty to record around the clock", in)
	}

	return &Window{Start: start, End: end}, nil
}

func parseTimeOfDay(in string) (time.Duration, error) {
	parts := strings.Split(in, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, fmt.Errorf("invalid time of day %q: must be HH:MM or HH:MM:SS", in)
	}

	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 || len(parts[0]) != 2 {
		return 0, fmt.Errorf("invalid hour in %q: must be between 00 and 23", in)
	}

	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("invalid minute in %q: must be between 00 and 59", in)
	}

	second := 0
	if len(parts) == 3 {
		second, err = strconv.Atoi(parts[2])
		if err != nil || second < 0 || second > 59 || len(parts[2]) != 2 {
			return 0, fmt.Errorf("invalid second in %q: must be between 00 and 59", in)
		}
	}

	return time.Duration(hour)*time.Hour +
		time.Duration(minute)*time.Minute +
		time.Duration(second)*time.Second, nil
}

// TimeOfDay returns the offset from UTC midnight of the given instant.
func TimeOfDay(t time.Time) time.Duration {
	t = t.UTC()
	return time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second +
		time.Duration(t.Nanosecond())
}

// Contains reports whether t belongs to the window.
// The start boundary is inclusive, the end boundary is exclusive, so a
// segment is stopped exactly when the window closes.
func (w *Window) Contains(t time.Time) bool {
	tod := TimeOfDay(t)
	if w.Start < w.End {
		return tod >= w.Start && tod < w.End
	}
	// the window crosses midnight: [Start, 24h) union [0, End)
	return tod >= w.Start || tod < w.End
}

// NextTransition returns the first instant strictly after from at which the
// inside/outside state changes, i.e. the next window start or end.
func (w *Window) NextTransition(from time.Time) time.Time {
	start := nextBoundary(from, w.Start)
	end := nextBoundary(from, w.End)
	if start.Before(end) {
		return start
	}
	return end
}

// nextBoundary returns the first occurrence of the given UTC time of day
// strictly after from.
func nextBoundary(from time.Time, tod time.Duration) time.Time {
	from = from.UTC()
	midnight := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	candidate := midnight.Add(tod)
	if !candidate.After(from) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}

// String implements fmt.Stringer and returns the canonical HH:MM-HH:MM form,
// or HH:MM:SS-HH:MM:SS when seconds are present.
func (w *Window) String() string {
	return formatTimeOfDay(w.Start) + "-" + formatTimeOfDay(w.End)
}

func formatTimeOfDay(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60
	if seconds != 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%02d:%02d", hours, minutes)
}
