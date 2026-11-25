// Package externalcmd allows to launch external commands.
package externalcmd

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	restartPause = 5 * time.Second
)

var (
	errTerminated       = errors.New("terminated")
	errRestartRequested = errors.New("restart requested")
)

// OnExitFunc is the prototype of onExit.
type OnExitFunc func(error)

// Environment is a Cmd environment.
type Environment map[string]string

// Cmd is an external command.
type Cmd struct {
	Pool    *Pool
	Cmdstr  string
	Restart bool
	Env     Environment
	OnStart func()
	OnStop  func()
	OnExit  OnExitFunc

	// RestartPause is the pause before a restart. It defaults to 5 seconds.
	RestartPause time.Duration

	// in
	terminate chan struct{}

	procMutex   sync.Mutex
	procRestart chan struct{}
}

// Start starts the command.
func (c *Cmd) Start() {
	if c.OnExit == nil {
		c.OnExit = func(_ error) {}
	}

	if c.RestartPause == 0 {
		c.RestartPause = restartPause
	}

	c.terminate = make(chan struct{})

	c.Pool.wg.Add(1)

	go c.run()
}

// Close closes the command. It doesn't wait for the command to exit.
func (c *Cmd) Close() {
	close(c.terminate)
}

// Terminate terminates the process that is currently running.
// If Restart is enabled, the command is started again after the restart pause;
// otherwise the command exits. Unlike Close(), this does not stop the restart
// loop. It is a no-op if there is no running process to terminate.
func (c *Cmd) Terminate() {
	c.procMutex.Lock()
	restart := c.procRestart
	c.procRestart = nil
	c.procMutex.Unlock()

	if restart != nil {
		close(restart)
	}
}

func expandEnv(s string, env Environment) string {
	return os.Expand(s, func(variable string) string {
		if value, ok := env[variable]; ok {
			return value
		}
		return os.Getenv(variable)
	})
}

func (c *Cmd) run() {
	defer c.Pool.wg.Done()

	env := append([]string(nil), os.Environ()...)
	for key, val := range c.Env {
		env = append(env, key+"="+val)
	}

	for {
		// do not start a new process if the command has been closed while
		// waiting for the restart pause.
		select {
		case <-c.terminate:
			return
		default:
		}

		restart := make(chan struct{})

		c.procMutex.Lock()
		c.procRestart = restart
		c.procMutex.Unlock()

		if c.OnStart != nil {
			c.OnStart()
		}

		err := c.runOSSpecific(c.Cmdstr, env, restart)

		if c.OnStop != nil {
			c.OnStop()
		}

		c.procMutex.Lock()
		if c.procRestart == restart {
			c.procRestart = nil
		}
		c.procMutex.Unlock()

		switch {
		case errors.Is(err, errTerminated):
			// Close() was called: stop definitely.
			return

		case errors.Is(err, errRestartRequested):
			// Terminate() was called: the caller is already aware of it and
			// logs it, hence OnExit is not called. Without Restart, stop.
			if !c.Restart {
				return
			}

		case !c.Restart:
			if err != nil {
				c.OnExit(err)
			}
			return

		default:
			if err != nil {
				c.OnExit(err)
			} else {
				c.OnExit(fmt.Errorf("command exited with code 0"))
			}
		}

		select {
		case <-time.After(c.RestartPause):
		case <-c.terminate:
			return
		}
	}
}
