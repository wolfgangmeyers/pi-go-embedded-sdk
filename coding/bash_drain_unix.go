//go:build linux || darwin

package coding

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Disable group cancellation before reaping, including os/exec's context watcher.
func guardBashCancel(cmd *exec.Cmd) func() {
	if cmd.Cancel == nil {
		return func() {}
	}
	cancel := cmd.Cancel
	var mu sync.Mutex
	active := true
	cmd.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		if !active {
			return os.ErrProcessDone
		}
		return cancel()
	}
	return func() { mu.Lock(); active = false; mu.Unlock() }
}

type bashDrainOps struct {
	setNonblock func(int, bool) error
	pipe        func() (*os.File, *os.File, error)
	read        func(int, []byte) (int, error)
	poll        func([]unix.PollFd, int) (int, error)
}

var defaultBashDrainOps = bashDrainOps{unix.SetNonblock, os.Pipe, unix.Read, unix.Poll}

// stopBashOnDrainFailure uses the command's cancellation policy when supplied.
func stopBashOnDrainFailure(cmd *exec.Cmd) {
	if cmd.Cancel != nil {
		_ = cmd.Cancel()
	} else {
		_ = cmd.Process.Kill()
	}
}

// drainBashPipe owns the reader until EOF or the bounded post-exit window.
// Callbacks are synchronous and can block indefinitely.
func drainBashPipe(cmd *exec.Cmd, pr *os.File, w io.Writer, finish func()) error {
	return drainBashPipeWithOpsFinish(cmd, pr, w, defaultBashDrainOps, finish)
}

func drainBashPipeWithOps(cmd *exec.Cmd, pr *os.File, w io.Writer, ops bashDrainOps) error {
	return drainBashPipeWithOpsFinish(cmd, pr, w, ops, func() {})
}

func drainBashPipeWithOpsFinish(cmd *exec.Cmd, pr *os.File, w io.Writer, ops bashDrainOps, finish func()) error {
	fd := int(pr.Fd())
	failSetup := func(err error) error {
		// Close before waiting: the child must not block on an undrained pipe.
		_ = pr.Close()
		stopBashOnDrainFailure(cmd)
		finish()
		return errors.Join(drainFailure(err), cmd.Wait())
	}
	if err := ops.setNonblock(fd, true); err != nil {
		return failSetup(fmt.Errorf("bash output nonblocking setup: %w", err))
	}
	wakeRead, wakeWrite, err := ops.pipe()
	if err != nil {
		return failSetup(fmt.Errorf("bash output wake pipe: %w", err))
	}
	var exitAt atomic.Pointer[time.Time]
	done := make(chan error, 1)
	go func() {
		defer pr.Close()
		defer wakeRead.Close()
		buf := make([]byte, 32*1024)
		var limit time.Time
		exited := false
		for {
			if !exited {
				if at := exitAt.Load(); at != nil {
					exited = true
					limit = at.Add(bashExitStdioLimit)
				}
			}
			// A final read preserves a buffered tail after a slow callback.
			hardExpired := exited && !time.Now().Before(limit)
			n, err := ops.read(fd, buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if hardExpired {
					done <- drainFailure(ErrBashOutputIncomplete)
					return
				}
				continue
			}
			if hardExpired && (errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK)) {
				done <- drainFailure(ErrBashOutputIncomplete)
				return
			}
			if err == nil {
				done <- nil
				return
			}
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
				done <- drainFailure(fmt.Errorf("bash output read: %w", err))
				return
			}
			timeout := -1
			if exited {
				remaining := time.Until(limit)
				if remaining <= 0 {
					done <- drainFailure(ErrBashOutputIncomplete)
					return
				}
				timeout = int((remaining + time.Millisecond - 1) / time.Millisecond)
			}
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if !exited {
				fds = append(fds, unix.PollFd{Fd: int32(wakeRead.Fd()), Events: unix.POLLIN})
			}
			_, err = ops.poll(fds, timeout)
			if err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}
				done <- drainFailure(fmt.Errorf("bash output poll: %w", err))
				return
			}
			if fds[0].Revents&unix.POLLERR != 0 {
				done <- drainFailure(fmt.Errorf("bash output poll: pipe error"))
				return
			}
			if fds[0].Revents&unix.POLLNVAL != 0 {
				done <- drainFailure(fmt.Errorf("bash output poll: %w", unix.EBADF))
				return
			}
			if !exited && fds[1].Revents != 0 {
				exited = true
				at := exitAt.Load()
				limit = at.Add(bashExitStdioLimit)
			}
		}
	}()
	// Observe exit without reaping: until Wait, the leader's PID remains
	// reserved, so cancellation on a drain failure cannot hit a reused group.
	exited := make(chan error, 1)
	go func() {
		err := observeBashExit(cmd.Process.Pid)
		now := time.Now()
		exitAt.Store(&now)
		exited <- err
	}()
	var drainErr error
	select {
	case err := <-exited:
		if err != nil {
			stopBashOnDrainFailure(cmd)
			drainErr = drainFailure(fmt.Errorf("bash exit observation: %w", err))
		}
		_ = wakeWrite.Close()
		readErr := <-done
		if readErr != nil && !errors.Is(readErr, ErrBashOutputIncomplete) {
			stopBashOnDrainFailure(cmd)
		}
		drainErr = errors.Join(drainErr, readErr)
	case drainErr = <-done:
		if drainErr != nil && !errors.Is(drainErr, ErrBashOutputIncomplete) {
			stopBashOnDrainFailure(cmd)
		}
		if err := <-exited; err != nil {
			stopBashOnDrainFailure(cmd)
			drainErr = errors.Join(drainErr, drainFailure(fmt.Errorf("bash exit observation: %w", err)))
		}
		_ = wakeWrite.Close()
	}
	finish()
	return errors.Join(cmd.Wait(), drainErr)
}
