//go:build linux || darwin

package coding

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

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

// drainBashPipe owns the reader until EOF or a post-exit idle interval.
// Callbacks are synchronous and can block indefinitely.
func drainBashPipe(cmd *exec.Cmd, pr *os.File, w io.Writer) error {
	return drainBashPipeWithOps(cmd, pr, w, defaultBashDrainOps)
}

func drainBashPipeWithOps(cmd *exec.Cmd, pr *os.File, w io.Writer, ops bashDrainOps) error {
	fd := int(pr.Fd())
	failSetup := func(err error) error {
		// Close before waiting: the child must not block on an undrained pipe.
		_ = pr.Close()
		stopBashOnDrainFailure(cmd)
		return errors.Join(drainFailure(err), cmd.Wait())
	}
	if err := ops.setNonblock(fd, true); err != nil {
		return failSetup(fmt.Errorf("bash output nonblocking setup: %w", err))
	}
	wakeRead, wakeWrite, err := ops.pipe()
	if err != nil {
		return failSetup(fmt.Errorf("bash output wake pipe: %w", err))
	}
	done := make(chan error, 1)
	go func() {
		defer pr.Close()
		defer wakeRead.Close()
		buf := make([]byte, 32*1024)
		var idleSince time.Time
		exited := false
		for {
			n, err := ops.read(fd, buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				idleSince = time.Time{}
				continue
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
				if idleSince.IsZero() {
					idleSince = time.Now()
				}
				remaining := bashExitStdioGrace - time.Since(idleSince)
				if remaining <= 0 {
					done <- nil
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
			}
		}
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr, drainErr error
	select {
	case waitErr = <-waited:
		// Closing the write end wakes poll, even if exit races the first poll.
		_ = wakeWrite.Close()
		drainErr = <-done
	case drainErr = <-done:
		if drainErr != nil {
			stopBashOnDrainFailure(cmd)
		}
		waitErr = <-waited
		_ = wakeWrite.Close()
	}
	if drainErr == nil {
		return waitErr
	}
	return errors.Join(waitErr, drainErr)
}
