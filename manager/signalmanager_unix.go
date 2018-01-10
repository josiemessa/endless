// +build !windows

package manager

import (
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/stevenh/endless"
)

// SignalHook represents a signal processing hook.
// If false is returned no further processing of the signal is performed.
type SignalHook func(sig os.Signal) (cont bool)

// SignalManager listens for signals and takes action on the Server.
//
// By default:
// SIGHUP: calls Restart()
// SIGUSR2: calls Terminate(0), Shutdown() must have been called first.
// SIGINT & SIGTERM: calls Shutdown()
//
// Pre and post signal handles can also be registered for custom actions.
type SignalManager struct {
	mtx       sync.Mutex
	done      chan struct{}
	wg        *sync.WaitGroup
	preHooks  map[os.Signal][]SignalHook
	postHooks map[os.Signal][]SignalHook
}

// NewSignalManager create a new SignalManager for the s
func NewSignalManager() *SignalManager {
	return &SignalManager{
		done:      make(chan struct{}),
		wg:        &sync.WaitGroup{},
		preHooks:  make(map[os.Signal][]SignalHook),
		postHooks: make(map[os.Signal][]SignalHook),
	}
}

// Stop stops the handler from taking any more action
func (m *SignalManager) Stop() {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	select {
	case <-m.done:
		// already closed
		return
	default:
		close(m.done)
	}
}

// Manage listens for os.Signal's and calls any registered function hooks.
func (m *SignalManager) Manage(srv *endless.Server) {
	c := make(chan os.Signal, 1)
	signal.Notify(c)
	defer func() {
		signal.Stop(c)
		close(m.done)
	}()

	pid := syscall.Getpid()
	for {
		var sig os.Signal
		select {
		case sig = <-c:
		case <-m.done:
		}

		if !m.handleSignal(m.preHooks[sig], sig) {
			continue
		}

		switch sig {
		case syscall.SIGHUP:
			srv.Debugln("Received", sig, "restarting...")
			if _, err := srv.Restart(); err != nil {
				srv.Println("Fork err:", err)
			}
		case syscall.SIGUSR2:
			srv.Debugln("Received", sig, "terminating...")
			if err := srv.Terminate(0); err != nil {
				srv.Println(pid, err)
			}
		case syscall.SIGINT, syscall.SIGTERM:
			srv.Debugln("Received", sig, "shutting down...")
			if err := srv.Shutdown(); err != nil {
				srv.Println(pid, err)
			}
		}

		m.handleSignal(m.postHooks[sig], sig)
	}
}

// handleSignal calls all hooks for a signal.
// Returns false early if a hook returns false.
func (m *SignalManager) handleSignal(hooks []SignalHook, sig os.Signal) bool {
	for _, f := range hooks {
		if !f(sig) {
			return false
		}
	}
	return true
}

// RegisterPreSignalHook registers a function to be run before any built in signal action.
func (m *SignalManager) RegisterPreSignalHook(sig os.Signal, f SignalHook) {
	m.preHooks[sig] = append(m.preHooks[sig], f)
}

// RegisterPostSignalHook registers a function to be run after any built in signal action.
func (m *SignalManager) RegisterPostSignalHook(sig os.Signal, f SignalHook) {
	m.preHooks[sig] = append(m.preHooks[sig], f)
}

type ServerManager struct {
	endless.Server
}
