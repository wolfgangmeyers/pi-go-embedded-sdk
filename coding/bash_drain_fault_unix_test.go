//go:build linux || darwin

package coding

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBashDrainFaults(t *testing.T) {
	sentinel := errors.New("injected fault")
	for _, stage := range []string{"nonblock", "pipe", "read", "poll"} {
		t.Run(stage, func(t *testing.T) {
			pr, pw, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", "sleep 0.1; printf output")
			cmd.Stdout = pw
			cmd.Stderr = pw
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pw.Close()
			ops := defaultBashDrainOps
			switch stage {
			case "nonblock":
				ops.setNonblock = func(int, bool) error { return sentinel }
			case "pipe":
				ops.pipe = func([]int) error { return sentinel }
			case "read":
				ops.read = func(int, []byte) (int, error) { return 0, sentinel }
			case "poll":
				ops.poll = func([]unix.PollFd, int) (int, error) { return 0, sentinel }
			}
			done := make(chan error, 1)
			go func() { done <- drainBashPipeWithOps(cmd, pr, io.Discard, ops) }()
			select {
			case err := <-done:
				if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "bash output") {
					t.Fatalf("error = %v", err)
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("drain hung")
			}
		})
	}
}

func TestBashDrainWakeAndExitStatus(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "printf hello; exit 7")
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	var out bytes.Buffer
	err = drainBashPipe(cmd, pr, &out)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || out.String() != "hello" {
		t.Fatalf("output %q error %v", out.String(), err)
	}
}
