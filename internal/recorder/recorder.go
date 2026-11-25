// Package recorder contains the recorder.
package recorder

import (
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/recordwindow"
	"github.com/bluenviron/mediamtx/internal/stream"
)

const (
	ntpDriftTolerance = 5 * time.Second
)

// OnSegmentCreateFunc is the prototype of the function passed as OnSegmentCreate
type OnSegmentCreateFunc = func(path string)

// OnSegmentCompleteFunc is the prototype of the function passed as OnSegmentComplete
type OnSegmentCompleteFunc = func(path string, duration time.Duration)

// Recorder writes recordings to disk.
//
// If Window is nil, recording is always active, which is the legacy behavior.
// If Window is set, an instance is started and stopped automatically at the
// daily UTC window boundaries while the underlying stream stays online:
// stopping an instance finalizes the current segment without disconnecting
// the publisher.
type Recorder struct {
	PathFormat        string
	Format            conf.RecordFormat
	PartDuration      time.Duration
	MaxPartSize       conf.StringSize
	SegmentDuration   time.Duration
	PathName          string
	Stream            *stream.Stream
	Window            *recordwindow.Window
	Clock             Clock
	OnSegmentCreate   OnSegmentCreateFunc
	OnSegmentComplete OnSegmentCompleteFunc
	Parent            logger.Writer

	restartPause time.Duration
	location     *time.Location

	// onInstanceReady, when set, is called after a new recorder instance has
	// been initialized and its reader has been registered with the stream.
	// It is used by tests to synchronize writes with instance restarts.
	onInstanceReady func()

	currentInstance *recorderInstance

	terminate chan struct{}
	done      chan struct{}
}

// Initialize initializes Recorder.
func (r *Recorder) Initialize() {
	if r.OnSegmentCreate == nil {
		r.OnSegmentCreate = func(string) {
		}
	}
	if r.OnSegmentComplete == nil {
		r.OnSegmentComplete = func(string, time.Duration) {
		}
	}
	if r.restartPause == 0 {
		r.restartPause = 2 * time.Second
	}
	if r.location == nil {
		r.location = time.Local
	}
	if r.Clock == nil {
		r.Clock = systemClock{}
	}

	r.terminate = make(chan struct{})
	r.done = make(chan struct{})

	// Create the first instance synchronously when we are inside the window,
	// like in the window-less case, so that an unsupported stream is reported
	// immediately. Outside the window the recorder stays idle until the next
	// window start.
	if r.shouldRecord(r.Clock.Now()) {
		r.currentInstance = r.newInstance()
	} else if r.Window != nil {
		r.Log(logger.Info, "outside recording window %s, recording paused", r.Window.String())
	}

	go r.run()
}

// Log implements logger.Writer.
func (r *Recorder) Log(level logger.Level, format string, args ...any) {
	r.Parent.Log(level, "[recorder] "+format, args...)
}

// Close closes the agent and finalizes any segment being written.
func (r *Recorder) Close() {
	r.Log(logger.Info, "recording stopped")
	close(r.terminate)
	<-r.done
}

func (r *Recorder) newInstance() *recorderInstance {
	ri := &recorderInstance{
		pathFormat:        r.PathFormat,
		format:            r.Format,
		partDuration:      r.PartDuration,
		maxPartSize:       r.MaxPartSize,
		segmentDuration:   r.SegmentDuration,
		pathName:          r.PathName,
		stream:            r.Stream,
		onSegmentCreate:   r.OnSegmentCreate,
		onSegmentComplete: r.OnSegmentComplete,
		location:          r.location,
		parent:            r,
	}
	ri.initialize()
	if r.onInstanceReady != nil {
		r.onInstanceReady()
	}
	return ri
}

// shouldRecord reports whether an instance must be active at time t.
// A nil window means recording is always allowed.
func (r *Recorder) shouldRecord(t time.Time) bool {
	return r.Window == nil || r.Window.Contains(t)
}

func (r *Recorder) run() {
	defer close(r.done)

	// instanceDone is nil when no instance is running: receiving from a nil
	// channel blocks forever, which keeps the select valid while idle.
	var instanceDone <-chan struct{}
	if r.currentInstance != nil {
		instanceDone = r.currentInstance.done
		if r.Window != nil {
			r.Log(logger.Info, "inside recording window %s, recording started", r.Window.String())
		}
	}

	var boundaryTimer Timer
	var boundaryC <-chan time.Time

	stopBoundaryTimer := func() {
		if boundaryTimer != nil {
			boundaryTimer.Stop()
			boundaryTimer = nil
			boundaryC = nil
		}
	}

	// armBoundaryTimer (re)arms the timer for the next window transition and
	// exposes its channel. With no window configured it stays disabled.
	armBoundaryTimer := func() {
		stopBoundaryTimer()
		if r.Window == nil {
			return
		}
		now := r.Clock.Now()
		d := r.Window.NextTransition(now).Sub(now)
		if d <= 0 {
			// A clock may report the boundary as already reached between the
			// two calls (or be controlled by tests); ask again from a strictly
			// later reference instead of arming a non-positive timer.
			d = time.Nanosecond
		}
		boundaryTimer = r.Clock.NewTimer(d)
		boundaryC = boundaryTimer.C()
	}

	armBoundaryTimer()

	var restartTimer *time.Timer
	var restartC <-chan time.Time

	stopRestartTimer := func() {
		if restartTimer != nil {
			restartTimer.Stop()
			restartTimer = nil
			restartC = nil
		}
	}

	for {
		select {
		case <-instanceDone:
			// The current instance terminated on its own (write error or time
			// drift reset): finalize it and restart after the usual pause,
			// unless the window has meanwhile closed.
			r.currentInstance.close()
			r.currentInstance = nil
			instanceDone = nil

			// The restart pause always uses the real clock: it is an
			// internal retry delay, not a window boundary.
			restartTimer = time.NewTimer(r.restartPause)
			restartC = restartTimer.C

		case <-restartC:
			stopRestartTimer()

			// A boundary may have been crossed during the restart pause.
			// Re-arm from the current time and re-evaluate the state.
			armBoundaryTimer()

			if r.currentInstance == nil && r.shouldRecord(r.Clock.Now()) {
				r.currentInstance = r.newInstance()
				instanceDone = r.currentInstance.done
			}

		case <-boundaryC:
			// A boundary reconciliation supersedes any pending restart pause.
			stopRestartTimer()

			// A daily window boundary was reached. Re-arm first so the next
			// transition is already scheduled, then reconcile the instance
			// with the current window state.
			armBoundaryTimer()

			if r.shouldRecord(r.Clock.Now()) {
				if r.currentInstance == nil {
					r.Log(logger.Info, "recording window %s opened, recording started", r.Window.String())
					r.currentInstance = r.newInstance()
					instanceDone = r.currentInstance.done
				}
			} else if r.currentInstance != nil {
				r.Log(logger.Info, "recording window %s closed, finalizing segment", r.Window.String())
				r.currentInstance.close()
				r.currentInstance = nil
				instanceDone = nil
			}

		case <-r.terminate:
			stopRestartTimer()
			stopBoundaryTimer()
			if r.currentInstance != nil {
				r.currentInstance.close()
				r.currentInstance = nil
			}
			return
		}
	}
}
