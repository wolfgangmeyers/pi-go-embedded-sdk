package coding

import (
	"io"
	"os"
	"os/exec"
	"time"
)

// drainBashPipeFallback retains the original behavior on unsupported platforms
// only.
func drainBashPipeFallback(cmd *exec.Cmd, pr *os.File, w io.Writer) error {
	chunks := make(chan struct{}, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
			}
			select {
			case chunks <- struct{}{}:
			default:
			}
			if err != nil {
				return
			}
		}
	}()
	waitErr := cmd.Wait()
	timer := time.NewTimer(bashExitStdioGrace)
	defer timer.Stop()
	for {
		select {
		case <-readDone:
			pr.Close()
			return waitErr
		case <-chunks:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(bashExitStdioGrace)
		case <-timer.C:
			pr.Close()
			<-readDone
			return waitErr
		}
	}
}
