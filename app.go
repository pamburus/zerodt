// Copyright 2017 Grigory Zubankov. All rights reserved.
// Use of this source code is governed by a MIT license
// that can be found in the LICENSE file.
//
// +build linux darwin

package zerodt

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var (
	// Get original working directory just on start to reduce
	// possibility of calling `os.Chdir` by somebody.
	originalWD, _ = os.Getwd()
)

// App specifies functions to control passed HTTP servers.
type App struct {
	// PreServeFn is a common hook notifies client that all servers are
	// about to start serving.
	PreServeFn func(inherited bool) error

	// CompleteShutdownFn is a parent's hook, a part of shutdown process
	// that allows client to do extra work after all http servers will
	// be shutdown. All dependent resources can be closed here.
	//
	// For stateful services: and before child will start serving.
	CompleteShutdownFn func()

	// PreParentExitFn is a child's hook that allows client to do
	// extra work on a child's side before the parent will exit.
	//
	// Useful e.g. for updating pid in a pid file while acting
	// as a systemd's service.
	PreParentExitFn func()

	served                    sync.WaitGroup
	servers                   []*http.Server
	waitParentShutdownTimeout time.Duration
	waitChildTimeout          time.Duration
}

// NewApp returns a new App instance.
func NewApp(servers ...*http.Server) *App {
	a := &App{
		PreServeFn:                func(inherited bool) error { return nil },
		CompleteShutdownFn:        func() {},
		PreParentExitFn:           func() {},
		servers:                   servers,
		waitChildTimeout:          time.Second * 60,
		waitParentShutdownTimeout: 0,
	}
	// Need to be sure all servers are serving before calling shutdown.
	a.served.Add(len(a.servers))
	return a
}

// SetWaitChildTimeout sets the maximum amount of time for a parent
// to wait for a child when activation is started. It is reset whenever
// a new activation process is started.
//
// When the timeout ends, the activating child will be killed with
// no regrets. The activation prosess will be stopped in this case.
//
// There is only one reason to tune this timeout - if the app is
// starting for a long time.
//
// Default value is 60 seconds.
func (a *App) SetWaitChildTimeout(d time.Duration) {
	a.waitChildTimeout = d
}

// SetWaitParentShutdownTimeout sets the maximum amount of time for a
// child to wait for a parent shutdown when activation is started. It
// is reset whenever a new activation process is started.
//
// When the timeout ends (if it is not 0), the activated child will
// kill his parent.
//
// The timeout is usable for statefull services and basically describes
// maximum amount of time for a single request handling by a parent.
//
// Default value is 0 that means no timeout. A child will start
// accepting new connections immediately.
func (a *App) SetWaitParentShutdownTimeout(d time.Duration) {
	a.waitParentShutdownTimeout = d
}

// Shutdown gracefully shut downs all servers without interrupting any
// active connections.
func (a *App) Shutdown() {
	// Wait for all servers to start serving to avoid race conditions
	// connected with shutdown. 'Shutdown' must be called only if server
	// has already started or it does nothing.
	logger.Printf("ZeroDT: shutdown servers...")
	a.served.Wait()

	var wg sync.WaitGroup
	wg.Add(len(a.servers))

	// Shutdown all servers in parallel
	for _, s := range a.servers {
		go func(s *http.Server) {
			defer wg.Done()
			err := s.Shutdown(context.Background())
			if err != nil {
				logger.Printf("ZeroDT: server %s has been shutdown with: %v", s.Addr, err)
				return
			}
			logger.Printf("ZeroDT: server %s has been shutdown", s.Addr)
		}(s)
	}

	wg.Wait()
	a.CompleteShutdownFn()
}

// ListenAndServe creates listeners for the given servers or reuses
// the inherited ones. It also serves the servers and monitors OS
// signals.
func (a *App) ListenAndServe() error {
	inherited, messenger, err := inherit()
	if err != nil {
		logger.Printf("ZeroDT: failed to inherit listeners with: '%v'", err)
		return err
	}
	e := newExchange(inherited)
	logger.Printf("ZeroDT: serving with pid='%d', inherited='%s'", os.Getpid(), formatInherited(e))

	// Signals wait group.
	var sigWG sync.WaitGroup
	sigWG.Add(1)
	sigCtx, sigCancelFunc := context.WithCancel(context.Background())
	go a.handleSignals(sigCtx, &sigWG, e)

	// Servers 'Listen' wait group.
	var startWG sync.WaitGroup
	startWG.Add(len(a.servers))
	// Servers 'Serve' channel.
	finishCh := make(chan error, len(a.servers))
	// Waiting for parent wait group.
	var parentWG sync.WaitGroup
	parentWG.Add(1)

	var startErr error

	for _, s := range a.servers {
		go func(s *http.Server) {
			var err error
			defer func() { finishCh <- err }()
			// Make sure Shutdown is not blocked event if
			// notifyListener.Accept() not call.
			servedOnce := &doneOnce{wg: &a.served}
			defer servedOnce.Done()
			// Make sure startWG.Wait() is not blocked in case of error
			// in acquireOrCreateListener.
			startOnce := &doneOnce{wg: &startWG}
			defer startOnce.Done()

			l, err := e.acquireOrCreateListener("tcp", s.Addr)
			if err != nil {
				logger.Printf("ZeroDT: failed to listen on %v with: %v", s.Addr, err)
				return
			}
			// A server is about to Serve and already listen.
			startOnce.Done()
			// Wait for parent to start if set.
			parentWG.Wait()
			if startErr != nil {
				logger.Printf("ZeroDT: server %v exited with: %v", s.Addr, startErr)
				return
			}
			// TODO: shutdown all servers in case of error
			err = s.Serve(&notifyListener{Listener: tcpKeepAliveListener{l}, doneOnce: servedOnce})
			logger.Printf("ZeroDT: server %v has finished serving with: %v", s.Addr, err)
		}(s)
	}

	// Wait for all listeners to start listening.
	startWG.Wait()

	if messenger != nil {
		startErr = protocolActAsChild(messenger, a.waitChildTimeout, a.waitParentShutdownTimeout, a.PreParentExitFn)
	}
	if startErr == nil {
		startErr = a.PreServeFn(e.didInherit())
	}

	// Allow serverse's goroutines to start serving.
	parentWG.Done()

	// Wait for all server's. They may fail or be stopped by calling Shutdown.
	finalErr := startErr
	if finalErr != nil {
		sigCancelFunc()
	}
	for range a.servers {
		err = <-finishCh
		if finalErr == nil && err != nil && err != http.ErrServerClosed {
			sigCancelFunc()
			finalErr = err
		}
	}
	sigCancelFunc()
	sigWG.Wait()

	return finalErr
}

func (a *App) handleSignals(ctx context.Context, wg *sync.WaitGroup, e *exchange) {
	defer logger.Printf("ZeroDT: stop handling signals")
	defer wg.Done()

	signals := make(chan os.Signal, 10)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR2)
	defer signal.Stop(signals)

	wasShutdown := false

CatchSignals:
	for {
		select {
		// Exit.
		case <-ctx.Done():
			if !wasShutdown {
				// Possbile in case of errors in 'http.Serve'.
				// It's needed to start shutdown process any way.
				a.Shutdown()
			}
			return
		// OS signal.
		case s := <-signals:
			logger.Printf("ZeroDT: %v signal...", s)
			switch s {
			// Shutdown servers. No exit here.
			case syscall.SIGINT, syscall.SIGTERM:
				a.Shutdown()
				wasShutdown = true
			// Fork/Exec a child and shutdown.
			case syscall.SIGUSR2:
				_, f, err := forkExec(e.activeFiles())
				if err != nil {
					logger.Printf("ZeroDT: failed to forkExec: '%v'", err)
					continue CatchSignals
				}
				m, err := ListenSocket(f)
				if err != nil {
					logger.Printf("ZeroDT: failed to listen communication socket: '%v'", err)
					continue CatchSignals
				}
				// Nothing to do with errors.
				protocolActAsParent(m, a.waitChildTimeout, a.waitParentShutdownTimeout, func() {
					a.Shutdown()
					wasShutdown = true
				})
			}
		}
	}
}

// forkExec starts another process of yourself and passes the active
// listeners to a child to perform socket activation.
func forkExec(files []*os.File) (int, *os.File, error) {
	// Get the path name for the executable that started the current process.
	path, err := os.Executable()
	if err != nil {
		return -1, nil, err
	}
	// @TODO: remove
	// Fix the path name after the evaluation of any symbolic links.
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return -1, nil, err
	}

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, nil, err
	}
	f0 := os.NewFile(uintptr(fds[0]), "s|0")
	f1 := os.NewFile(uintptr(fds[1]), "s|1")
	files = append(files, f1)

	// Start the original executable with the original working directory.
	process, err := os.StartProcess(path, os.Args, &os.ProcAttr{
		Dir:   originalWD,
		Env:   prepareEnv(len(files)),
		Files: append([]*os.File{os.Stdin, os.Stdout, os.Stderr}, files...),
	})
	if err != nil {
		return -1, nil, err
	}

	return process.Pid, f0, nil
}

// formatInherited prints info about inherited listeners to a string.
func formatInherited(e *exchange) string {
	result := "["
	for i, pr := range e.inherited {
		if i != 0 {
			result += ", "
		}
		result += fmt.Sprintf("%v", pr.l.Addr())
	}
	result += "]"
	return result
}

type readyMsg struct {
	WaitParentShutdownTimeout time.Duration
}

type readyConfirmationMsg struct {
	FixedWaitParentShutdownTimeout time.Duration
}

type acceptedMsg struct {
}

type shutdownConfirmationMsg struct {
}

const (
	// Const timeout for Send operations.
	//
	// Socket buffer is big enough to keep our micro messages. So there
	// is no need to use long timeouts.
	sendTimeout = time.Second * 20
)

func maxTimeout(l time.Duration, r time.Duration) time.Duration {
	if l >= r {
		return l
	}
	return r
}

func protocolActAsParent(m *StreamMessenger, waitChildTimeout time.Duration, waitParentShutdownTimeout time.Duration, shutdownFn func()) error {
	defer m.Close()
	// Set deadline for ready/confirmation.
	m.SetDeadline(time.Now().Add(waitChildTimeout))

	// Child->Parent, ready message.
	logger.Printf("ZeroDT: waiting for readyMsg...")
	r := readyMsg{}
	err := m.Recv(&r)
	if err != nil {
		logger.Printf("ZeroDT: Parent<=>Child communication failed with: '%v'", err)
		// The child will die by timout.
		return err
	}

	// Parent->Child, ready confirmation message.
	logger.Printf("ZeroDT: sending readyConfirmationMsg to the child...")
	tipTimeout := maxTimeout(r.WaitParentShutdownTimeout, waitParentShutdownTimeout)
	err = m.Send(readyConfirmationMsg{FixedWaitParentShutdownTimeout: tipTimeout})
	if err != nil {
		logger.Printf("ZeroDT: Parent<=>Child communication failed with: '%v'", err)
		// The child will die by timout.
		return err
	}

	//
	// Ball is in child's court now. No error can stop parent to shutdown.
	//

	// Child->Parent, accepted message.
	logger.Printf("ZeroDT: waiting for acceptedMsg...")
	a := acceptedMsg{}
	err = m.Recv(&a)
	if err != nil {
		logger.Printf("ZeroDT: Parent<=>Child communication failed with: '%v'", err)
	}

	// Shutdown callback.
	shutdownFn()

	// Parent->Child, shutdown confirmation message.
	if tipTimeout == 0 {
		return nil
	}
	logger.Printf("ZeroDT: sending shutdownConfirmationMsg to the child...")
	m.SetDeadline(time.Now().Add(sendTimeout))
	err = m.Send(shutdownConfirmationMsg{})
	if err != nil {
		logger.Printf("ZeroDT: Parent<=>Child communication failed with: '%v'", err)
	}
	return nil
}

func protocolActAsChild(m *StreamMessenger, waitChildTimeout time.Duration, waitParentShutdownTimeout time.Duration, notifyFn func()) error {
	defer m.Close()

	// Child->Parent, ready message.
	logger.Printf("ZeroDT: sending readyMsg to the parent...")
	m.SetDeadline(time.Now().Add(sendTimeout))
	err := m.Send(readyMsg{WaitParentShutdownTimeout: waitParentShutdownTimeout})
	if err != nil {
		logger.Printf("ZeroDT: Parent<=>Child communication failed with: '%v'", err)
		return err
	}

	// Parent->Child, ready confirmation message.
	logger.Printf("ZeroDT: waiting for readyConfirmationMsg...")
	rcr := readyConfirmationMsg{}
	m.SetDeadline(time.Now().Add(maxTimeout(waitChildTimeout, waitParentShutdownTimeout)))
	err = m.Recv(&rcr)
	if err != nil {
		logger.Printf("ZeroDT: Parent<=>Child communication failed with: '%v'", err)
		return err
	}

	//
	// Ball is in our court now. The parent must die.
	//

	notifyFn()

	// Child->Parent, accepted message.
	logger.Printf("ZeroDT: sending acceptedMsg...")
	m.SetDeadline(time.Now().Add(sendTimeout))
	err = m.Send(acceptedMsg{})
	if err != nil {
		logger.Printf("ZeroDT: Parent<=>Child communication failed with: '%v'", err)
	}

	if rcr.FixedWaitParentShutdownTimeout == 0 {
		return nil
	}

	// Parent->Child, shutdown confirmation message.
	logger.Printf("ZeroDT: waiting for shutdownConfirmationMsg...")
	scr := shutdownConfirmationMsg{}
	m.SetDeadline(time.Now().Add(rcr.FixedWaitParentShutdownTimeout))
	err = m.Recv(&scr)
	if err != nil {
		logger.Printf("ZeroDT: Parent<=>Child communication failed with: '%v'", err)
		if opErr, ok := err.(*net.OpError); ok {
			if opErr.Timeout() {
				// There are issues on parent's side probably. Need to kill parent.
				parentPID, err := killParent()
				logger.Printf("ZeroDT: parent %d was killed with: '%v'", parentPID, err)
				return nil
			}
		}
		return err
	}
	// Everything is Ok.
	return nil
}

func killParent() (parentPID int, err error) {
	return killProcess(os.Getppid())
}

func killProcess(pid int) (parentPID int, err error) {
	// If it's systemd - keep it alive. Possible e.g. when systemd
	// performs 'socket activation'.
	if pid == 1 {
		return pid, fmt.Errorf("failed to kill process. It's systemd")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	err = p.Signal(syscall.SIGKILL)
	if err != nil {
		return
	}
	_, err = p.Wait()
	return
}
