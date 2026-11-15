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
	pipe        func([]int) error
	read        func(int, []byte) (int, error)
	poll        func([]unix.PollFd, int) (int, error)
}

var defaultBashDrainOps = bashDrainOps{unix.SetNonblock, unix.Pipe, unix.Read, unix.Poll}

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
		return errors.Join(err, cmd.Wait())
	}
	if err := ops.setNonblock(fd, true); err != nil {
		return failSetup(fmt.Errorf("bash output nonblocking setup: %w", err))
	}
	wake := []int{-1, -1}
	if err := ops.pipe(wake); err != nil {
		return failSetup(fmt.Errorf("bash output wake pipe: %w", err))
	}
	done := make(chan error, 1)
	go func() {
		defer pr.Close()
		defer unix.Close(wake[0])
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
				done <- fmt.Errorf("bash output read: %w", err)
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
				fds = append(fds, unix.PollFd{Fd: int32(wake[0]), Events: unix.POLLIN})
			}
			_, err = ops.poll(fds, timeout)
			if err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}
				done <- fmt.Errorf("bash output poll: %w", err)
				return
			}
			if fds[0].Revents&unix.POLLERR != 0 {
				done <- fmt.Errorf("bash output poll: pipe error")
				return
			}
			if fds[0].Revents&unix.POLLNVAL != 0 {
				done <- fmt.Errorf("bash output poll: %w", unix.EBADF)
				return
			}
			if !exited && fds[1].Revents != 0 {
				exited = true
			}
		}
	}()
	waitErr := cmd.Wait()
	// Closing the write end wakes poll, including when exit races the first poll.
	_ = unix.Close(wake[1])
	drainErr := <-done
	if drainErr == nil {
		return waitErr
	}
	return errors.Join(waitErr, drainErr)
}
