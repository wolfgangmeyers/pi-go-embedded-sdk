//go:build linux || darwin

package coding

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// drainBashPipe owns the reader until EOF or a post-exit idle interval. Check
// for readable bytes before observing idle: callback time must not discard a
// tail already buffered in the pipe. Callbacks run synchronously and can block
// indefinitely; neither cancellation nor the idle interval bounds them.
func drainBashPipe(cmd *exec.Cmd, pr *os.File, w io.Writer) error {
	fd := int(pr.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		return drainBashPipeFallback(cmd, pr, w)
	}
	exited := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pr.Close()
		buf := make([]byte, 32*1024)
		var idleSince time.Time
		for {
			n, err := unix.Read(fd, buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				// The idle period starts after delivery, not while the callback runs.
				idleSince = time.Time{}
				continue
			}
			if err == nil {
				return
			} // EOF
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
				return
			}
			// Check idleness only after a nonblocking read found no buffered bytes.
			timeout := 25 * time.Millisecond
			select {
			case <-exited:
				if idleSince.IsZero() {
					idleSince = time.Now()
				}
				remaining := bashExitStdioGrace - time.Since(idleSince)
				if remaining <= 0 {
					return
				}
				if remaining < timeout {
					timeout = remaining
				}
			default:
			}
			// Round up to avoid spinning in the final fractional millisecond.
			ms := int((timeout + time.Millisecond - 1) / time.Millisecond)
			_, pollErr := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, ms)
			if pollErr != nil && !errors.Is(pollErr, unix.EINTR) {
				return
			}
		}
	}()
	waitErr := cmd.Wait()
	close(exited)
	<-done
	return waitErr
}
