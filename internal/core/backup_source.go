package core

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/staticsources"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// BackupSourceState is the observable state of a backup source.
type BackupSourceState int

const (
	// BackupSourceStateIdle means the RTSP connection is not established yet,
	// or it is retrying after an error.
	BackupSourceStateIdle BackupSourceState = iota
	// BackupSourceStateReady means the RTSP connection is established and the
	// sub stream is parked behind the main publisher, ready to take over.
	BackupSourceStateReady
	// BackupSourceStateActive means the backup source is currently feeding
	// readers because the main publisher is offline.
	BackupSourceStateActive
)

// String implements fmt.Stringer.
func (s BackupSourceState) String() string {
	switch s {
	case BackupSourceStateReady:
		return "ready"
	case BackupSourceStateActive:
		return "active"
	default:
		return "idle"
	}
}

// backupRetryPause is the pause between backup connection attempts. It is
// shorter than the default static-source retry pause in order to reconnect
// quickly, while remaining gentle.
const backupRetryPause = 300 * time.Millisecond

// backupSourceManager keeps a RTSP backup source pre-connected and swaps it
// in when the main publisher disappears. State transitions are executed by the
// path goroutine; RTSP callbacks only forward requests to path channels.
type backupSourceManager struct {
	pa      *path
	handler *staticsources.Handler

	// sub is the parked/active sub stream of the current backup connection.
	sub atomic.Pointer[stream.SubStream]
}

func newBackupSourceManager(pa *path, pconf *conf.Path) *backupSourceManager {
	m := &backupSourceManager{pa: pa}

	handlerConf := pconf.Clone()
	handlerConf.Source = pconf.BackupSource

	m.handler = &staticsources.Handler{
		Conf:              handlerConf,
		LogLevel:          pa.logLevel,
		DumpPackets:       pa.dumpPackets,
		ReadTimeout:       pa.readTimeout,
		WriteTimeout:      pa.writeTimeout,
		WriteQueueSize:    pa.writeQueueSize,
		UDPReadBufferSize: pa.udpReadBufferSize,
		RTPMaxPayloadSize: pa.rtpMaxPayloadSize,
		SupportsIPv6:      pa.supportsIPv6,
		Matches:           pa.matches,
		PathManager:       pa.parent,
		Parent:            m,
		RetryPause:        backupRetryPause,
	}
	m.handler.Initialize()

	return m
}

func (m *backupSourceManager) start() {
	m.handler.Start(false, "")
}

// close stops the underlying static source and releases its resources.
func (m *backupSourceManager) close() {
	m.handler.Close("backup source removed or path is closing")
	if sub := m.sub.Swap(nil); sub != nil {
		sub.Close()
	}
}

// reloadConf forwards a configuration change to the underlying handler.
func (m *backupSourceManager) reloadConf(newConf *conf.Path) {
	handlerConf := newConf.Clone()
	handlerConf.Source = newConf.BackupSource
	m.handler.ReloadConf(handlerConf)
}

// State returns the current observable state. It is safe to call from any
// goroutine and derives the active/ready condition from the Stream itself.
func (m *backupSourceManager) State() BackupSourceState {
	sub := m.sub.Load()
	if sub == nil {
		return BackupSourceStateIdle
	}
	if sub.IsActive() {
		return BackupSourceStateActive
	}
	return BackupSourceStateReady
}

func (m *backupSourceManager) takeOverFromPublisher() bool {
	sub := m.sub.Load()
	if sub == nil {
		return false
	}

	if !sub.IsActive() {
		sub.Activate()
	}

	m.pa.Log(logger.Info, "main publisher is offline, switching to backup source")
	return true
}

// isActive reports whether the backup is currently serving readers.
func (m *backupSourceManager) isActive() bool {
	return m.State() == BackupSourceStateActive
}

// Log implements logger.Writer and staticsources.handlerParent.
func (m *backupSourceManager) Log(level logger.Level, format string, args ...any) {
	m.pa.Log(level, "[backup source] "+format, args...)
}

// StaticSourceHandlerSetReady implements staticsources.handlerParent.
// The request is forwarded to the path goroutine.
func (m *backupSourceManager) StaticSourceHandlerSetReady(
	ctx context.Context, req defs.PathSourceStaticSetReadyReq,
) {
	select {
	case m.pa.chBackupSetReady <- req:

	case <-m.pa.ctx.Done():
		req.Res <- defs.PathSourceStaticSetReadyRes{Err: fmt.Errorf("terminated")}

	case <-ctx.Done():
		req.Res <- defs.PathSourceStaticSetReadyRes{Err: fmt.Errorf("terminated")}
	}
}

// StaticSourceHandlerSetNotReady implements staticsources.handlerParent.
func (m *backupSourceManager) StaticSourceHandlerSetNotReady(
	ctx context.Context, req defs.PathSourceStaticSetNotReadyReq,
) {
	select {
	case m.pa.chBackupSetNotReady <- req:

	case <-m.pa.ctx.Done():
		close(req.Res)

	case <-ctx.Done():
		close(req.Res)
	}
}

// backupDescIsCompatible checks that the backup description contains a single
// H264 video track compatible with the main stream. The first supported version
// intentionally limits failover to this configuration.
func backupDescIsCompatible(backupDesc *description.Session, streamDesc *description.Session) error {
	if len(backupDesc.Medias) != 1 || len(streamDesc.Medias) != 1 {
		return fmt.Errorf("backup source must contain a single media")
	}

	backupMedia := backupDesc.Medias[0]
	streamMedia := streamDesc.Medias[0]

	if backupMedia.Type != description.MediaTypeVideo ||
		streamMedia.Type != description.MediaTypeVideo {
		return fmt.Errorf("backup source must contain a single video track")
	}

	if len(backupMedia.Formats) == 0 || len(streamMedia.Formats) == 0 {
		return fmt.Errorf("backup source has no formats")
	}

	if _, ok := backupMedia.Formats[0].(*format.H264); !ok {
		return fmt.Errorf("backup source codec is not H264")
	}

	if _, ok := streamMedia.Formats[0].(*format.H264); !ok {
		return fmt.Errorf("main stream codec is not H264")
	}

	return nil
}
