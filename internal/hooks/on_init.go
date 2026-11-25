package hooks

import (
	"sync/atomic"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/logger"
)

// OnInitParams are the parameters of OnInit.
type OnInitParams struct {
	Logger          logger.Writer
	ExternalCmdPool *externalcmd.Pool
	Conf            *conf.Path
	ExternalCmdEnv  externalcmd.Environment

	// OnStateChange is called whenever the command transitions between
	// running and not running. It can be nil.
	OnStateChange func()
}

// OnInitHandle is the handle of a runOnInit hook.
type OnInitHandle struct {
	logger  logger.Writer
	cmd     *externalcmd.Cmd
	restart bool
	running atomic.Bool
	onState func()
}

func (h *OnInitHandle) notifyStateChange() {
	if h.onState != nil {
		h.onState()
	}
}

// OnInit is the OnInit hook.
func OnInit(params OnInitParams) *OnInitHandle {
	h := &OnInitHandle{
		logger:  params.Logger,
		restart: params.Conf.RunOnInitRestart,
		onState: params.OnStateChange,
	}

	if params.Conf.RunOnInit != "" {
		params.Logger.Log(logger.Info, "runOnInit command started")
		h.cmd = &externalcmd.Cmd{
			Pool:    params.ExternalCmdPool,
			Cmdstr:  params.Conf.RunOnInit,
			Restart: params.Conf.RunOnInitRestart,
			Env:     params.ExternalCmdEnv,
			OnStart: func() {
				h.running.Store(true)
				h.notifyStateChange()
			},
			OnStop: func() {
				h.running.Store(false)
				h.notifyStateChange()
			},
			OnExit: func(err error) {
				params.Logger.Log(logger.Info, "runOnInit command exited: %v", err)
			},
		}
		h.cmd.Start()
	}

	return h
}

// Running reports whether a command process is currently running.
func (h *OnInitHandle) Running() bool {
	return h.running.Load()
}

// Terminate terminates the process that is currently running. When
// runOnInitRestart is enabled, the command is started again by the existing
// restart mechanism; otherwise it is not started again.
func (h *OnInitHandle) Terminate() {
	if h.cmd != nil && h.restart {
		h.cmd.Terminate()
	}
}

// Close definitely closes the hook and its command.
func (h *OnInitHandle) Close() {
	if h.cmd != nil {
		h.running.Store(false)
		h.cmd.Close()
		h.logger.Log(logger.Info, "runOnInit command stopped")
	}
}
