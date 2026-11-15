//go:build !linux && !darwin

package coding

import (
	"io"
	"os"
	"os/exec"
)

func drainBashPipe(cmd *exec.Cmd, pr *os.File, w io.Writer) error {
	return drainBashPipeFallback(cmd, pr, w)
}
