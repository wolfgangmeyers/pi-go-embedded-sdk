//go:build !linux && !darwin

package coding

import (
	"io"
	"os"
	"os/exec"
)

func guardBashCancel(cmd *exec.Cmd) func() { return func() {} }

func drainBashPipe(cmd *exec.Cmd, pr *os.File, w io.Writer, finish func()) error {
	return drainBashPipeFallback(cmd, pr, w)
}
