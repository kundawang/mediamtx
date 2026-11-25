package core

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/ratelimit"
	"github.com/bluenviron/mediamtx/internal/stream"
)

const (
	// publishBitrateWindow is the finite sliding time interval over which
	// received media bytes are summed to compute the sustained bitrate.
	publishBitrateWindow = 1 * time.Second

	// publishBitrateCheckInterval is how often the inbound byte counter is
	// sampled and fed to the sliding window.
	publishBitrateCheckInterval = 100 * time.Millisecond
)

// pathBitrateLimiter watches the inbound media byte rate of a single
// publisher and terminates it when the per-path sustained bitrate limit is
// exceeded.
//
// One instance is created for every successful AddPublisher request and
// destroyed when that publisher is removed, so switching publisher always
// starts from an empty window. It only reads the stream inbound byte counter
// (bytes received from the publisher); outbound bytes sent to readers are
// never counted, hence readers cannot influence the verdict.
type pathBitrateLimiter struct {
	pa     *path
	author defs.Publisher
	strm   *stream.Stream

	limiter *ratelimit.Limiter

	stopCh chan struct{}

	tripOnce sync.Once
	stopped  atomic.Bool
}

func (pa *path) startBitrateLimiter(author defs.Publisher, strm *stream.Stream) {
	bl := &pathBitrateLimiter{
		pa:      pa,
		author:  author,
		strm:    strm,
		limiter: ratelimit.New(0, publishBitrateWindow, time.Now),
		stopCh:  make(chan struct{}),
	}

	pa.bitrateLimiter = bl

	go bl.run()
}

// stop asks the watchdog to terminate. It never blocks on the watchdog, so it
// is safe to call from the path event loop even if terminating the publisher
// is waiting on path events.
func (bl *pathBitrateLimiter) stop() {
	if bl.stopped.CompareAndSwap(false, true) {
		close(bl.stopCh)
	}
}

func (pa *path) stopBitrateLimiter() {
	if bl := pa.bitrateLimiter; bl != nil {
		bl.stop()
		pa.bitrateLimiter = nil
	}
}

func (bl *pathBitrateLimiter) run() {
	ticker := time.NewTicker(publishBitrateCheckInterval)
	defer ticker.Stop()

	var (
		lastBytes int64
		haveLast  bool
	)

	for {
		select {
		case <-bl.stopCh:
			return

		case <-ticker.C:
		}

		pathConf := bl.pa.SafeConf()
		bytesPerSec, enabled := pathConf.MaxInboundBytesPerSec()

		if !enabled {
			// Unconfigured (or hot-reloaded to unlimited): never restrict
			// the stream and drop any accumulated history, so enabling the
			// limit later starts a fresh window.
			bl.limiter.Configure(0, publishBitrateWindow)
			bl.limiter.Reset()
			lastBytes = 0
			haveLast = false
			continue
		}

		bl.limiter.Configure(bytesPerSec, publishBitrateWindow)

		curBytes := int64(bl.strm.InboundBytes())

		if !haveLast || curBytes < lastBytes {
			// First sample, or a counter reset: establish a baseline without
			// charging pre-existing bytes to this observation interval.
			lastBytes = curBytes
			haveLast = true
			continue
		}

		delta := curBytes - lastBytes
		lastBytes = curBytes

		if bl.limiter.Add(delta) {
			bl.trip(bytesPerSec)
			return
		}
	}
}

func (bl *pathBitrateLimiter) trip(bytesPerSec int64) {
	bl.tripOnce.Do(func() {
		bl.pa.Log(logger.Warn,
			"publisher exceeded maximum sustained inbound bitrate "+
				"(limit: %d bytes/s over %s): terminating publisher",
			bytesPerSec, publishBitrateWindow)

		// Closing only the offending author makes every protocol server
		// run its regular disconnect/RemovePublisher path. Readers and other
		// paths are untouched by this direct action.
		bl.author.Close()
	})
}
