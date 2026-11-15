//go:build linux || darwin

package coding

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
			defer pr.Close()
			defer pw.Close()
			cmd := exec.Command("sh", "-c", "exec sleep 30")
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
				ops.pipe = func() (*os.File, *os.File, error) { return nil, nil, sentinel }
			case "read":
				ops.read = func(int, []byte) (int, error) { return 0, sentinel }
			case "poll":
				ops.read = func(int, []byte) (int, error) { return 0, unix.EAGAIN }
				ops.poll = func([]unix.PollFd, int) (int, error) { return 0, sentinel }
			}
			done := make(chan error, 1)
			go func() { done <- drainBashPipeWithOps(cmd, pr, io.Discard, ops) }()
			select {
			case err := <-done:
				var exit *exec.ExitError
				var drain *bashDrainError
				if !errors.Is(err, sentinel) || !errors.As(err, &exit) || !errors.As(err, &drain) || !strings.Contains(err.Error(), "bash output") {
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
	defer pr.Close()
	defer pw.Close()
	cmd := exec.Command("sh", "-c", "printf hello; exit 7")
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	var out bytes.Buffer
	err = drainBashPipe(cmd, pr, &out, func() {})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || out.String() != "hello" {
		t.Fatalf("output %q error %v", out.String(), err)
	}
}

func TestBashDrainFailureSurvivesExitClassification(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	waitErr := cmd.Run()
	sentinel := errors.New("read failed")
	joined := errors.Join(waitErr, drainFailure(sentinel))
	code, err := classifyShellRun(nil, nil, joined, 0)
	var exit *exec.ExitError
	var drain *bashDrainError
	if code != nil || !errors.As(err, &exit) || exit.ExitCode() != 7 || !errors.As(err, &drain) || !errors.Is(err, sentinel) {
		t.Fatalf("code %v, error %v", code, err)
	}
}

func TestBashWakePipeCloseOnExecConcurrentSpawn(t *testing.T) {
	stop := make(chan struct{})
	spawnDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				spawnDone <- nil
				return
			default:
				if err := exec.Command("true").Run(); err != nil {
					spawnDone <- err
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-spawnDone; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 20; i++ {
		r, w, err := defaultBashDrainOps.pipe()
		if err != nil {
			t.Fatal(err)
		}
		// Register cleanup before any setup assertion can fail.
		t.Cleanup(func() { r.Close(); w.Close() })
		for _, f := range []*os.File{r, w} {
			flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
			if err != nil || flags&unix.FD_CLOEXEC == 0 {
				t.Fatalf("fd flags %x: %v", flags, err)
			}
		}
		cmd := exec.Command("sleep", "10")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		w.Close()
		readDone := make(chan error, 1)
		go func() { _, err := io.Copy(io.Discard, r); readDone <- err }()
		select {
		case err := <-readDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("wake pipe inherited by child")
		}
		r.Close()
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
	}
}

func TestBashDrainFailureUsesCancellationPolicy(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	setProcessGroup(cmd)
	called := make(chan struct{}, 1)
	cmd.Cancel = func() error { called <- struct{}{}; return killProcessTree(cmd) }
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	ops := defaultBashDrainOps
	sentinel := errors.New("fault")
	ops.read = func(int, []byte) (int, error) { return 0, sentinel }
	err = drainBashPipeWithOps(cmd, pr, io.Discard, ops)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error %v", err)
	}
	select {
	case <-called:
	default:
		t.Fatal("cancellation policy not invoked")
	}
}

func TestBashDrainFailureAfterExitKillsGroupBeforeReap(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	// The inherited pipe independently witnesses the descendant closing on
	// cancellation. A controlled release and watchdog bound its lifetime.
	ackRead, ackWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ackRead.Close()
	defer ackWrite.Close()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	releaseWatchdog(t, release)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `(printf R >&3; touch "$1"; i=0; while [ ! -f "$2" ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done) & i=0; while [ ! -f "$1" ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done; exit 0`, "sh", ready, release)
	cmd.ExtraFiles = []*os.File{ackWrite}
	setProcessGroup(cmd)
	called := make(chan struct{}, 1)
	cmd.Cancel = func() error { called <- struct{}{}; return killProcessTree(cmd) }
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Until the drain takes ownership, always cancel and reap the leader on failure.
	owned := true
	defer func() {
		if owned {
			cancel()
			_ = cmd.Wait()
		}
	}()
	pw.Close()
	if err := ackWrite.Close(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- observeBashExit(cmd.Process.Pid) }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("leader exit observation timed out")
	}
	handshake := make(chan error, 1)
	go func() {
		b := make([]byte, 1)
		_, err := io.ReadFull(ackRead, b)
		if err == nil && b[0] != 'R' {
			err = errors.New("invalid witness handshake")
		}
		handshake <- err
	}()
	select {
	case err := <-handshake:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("witness handshake timed out")
	}
	if err := witnessAlive(ackRead); err != nil {
		t.Fatalf("witness exited before cancellation: %v", err)
	}
	closed := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, ackRead); closed <- err }()
	ops := defaultBashDrainOps
	sentinel := errors.New("after exit")
	ops.read = func(int, []byte) (int, error) { return 0, sentinel }
	err = drainBashPipeWithOps(cmd, pr, io.Discard, ops)
	owned = false
	if !errors.Is(err, sentinel) {
		t.Fatalf("error %v", err)
	}
	select {
	case <-called:
	default:
		t.Fatal("group cancellation not called")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("descendant retained its pipe after cancellation")
	}
}

func TestIncompleteDrainDoesNotHideOtherFailure(t *testing.T) {
	joined := errors.Join(drainFailure(ErrBashOutputIncomplete), drainFailure(errors.New("read failed")))
	if onlyIncompleteDrainErrors(joined) {
		t.Fatal("mixed drain failures accepted")
	}
}
