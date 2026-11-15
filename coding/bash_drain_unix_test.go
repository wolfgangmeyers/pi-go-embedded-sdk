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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// waitForFile is a hang guard, not a timing requirement for the behavior.
func waitForFile(t *testing.T, path string) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("timed out waiting for %s", path)
	return false
}

func signalFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Errorf("signal %s: %v", path, err)
	}
}

// releaseWatchdog releases gates on cleanup or a stalled test. Shell loops also
// have independent iteration bounds if their temporary directory is removed.
func releaseWatchdog(t *testing.T, paths ...string) {
	t.Helper()
	release := func() {
		for _, path := range paths {
			_ = os.WriteFile(path, nil, 0600)
		}
	}
	done := make(chan struct{})
	timer := time.AfterFunc(20*time.Second, func() { defer close(done); release() })
	t.Cleanup(func() {
		if !timer.Stop() {
			<-done
		}
		release()
	})
}

// witnessAlive checks the descriptor itself, not whether a reader goroutine
// happened to be scheduled. The handshake must already have been consumed.
func witnessAlive(f *os.File) error {
	p := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(p, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n != 0 {
			return errors.New("witness descriptor is readable or closed")
		}
		return nil
	}
}

func TestBashDrainSlowCallbackPreservesBufferedTail(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	exited := filepath.Join(dir, "exited")
	release := filepath.Join(dir, "release")
	descendantDone := filepath.Join(dir, "descendant-done")
	releaseWatchdog(t, release)
	defer func() { signalFile(t, release); waitForFile(t, descendantDone) }()
	// The descendant retains the pipe while the callback is blocked.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `
		(i=0; while [ ! -f "$3" ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done; touch "$4") &
		printf head; i=0; while [ ! -f "$1" ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done; printf tail; touch "$2"
	`, "sh", gate, exited, release, descendantDone)
	var output bytes.Buffer
	first := true
	err := runBashCommand(cmd, writerFunc(func(p []byte) (int, error) {
		output.Write(p)
		if first {
			first = false
			signalFile(t, gate)
			if waitForFile(t, exited) {
				// Make the callback exceed the post-exit window while the
				// tail is already buffered and the descendant holds the pipe.
				time.Sleep(2 * bashExitStdioGrace)
			}
			signalFile(t, release)
		}
		return len(p), nil
	}))
	if err != nil || output.String() != "headtail" {
		t.Fatalf("output %q, err %v", output.String(), err)
	}
}

func TestBashDrainDescendantIdleAndOutput(t *testing.T) {
	t.Run("quiet", func(t *testing.T) {
		dir := t.TempDir()
		release := filepath.Join(dir, "release")
		exited := filepath.Join(dir, "exited")
		releaseWatchdog(t, release)
		defer func() { signalFile(t, release); waitForFile(t, exited) }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		var out bytes.Buffer
		go func() {
			done <- runBashCommand(exec.CommandContext(ctx, "sh", "-c", `(i=0; while [ ! -f "$1" ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done; touch "$2") & printf done`, "sh", release, exited), &out)
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrBashOutputIncomplete) || out.String() != "done" {
				t.Fatalf("output %q err %v", out.String(), err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("quiet pipe was not released")
		}
	})
	t.Run("active", func(t *testing.T) {
		dir := t.TempDir()
		releaseWatchdog(t, filepath.Join(dir, "a"), filepath.Join(dir, "b"))
		defer func() {
			signalFile(t, filepath.Join(dir, "a"))
			signalFile(t, filepath.Join(dir, "b"))
			waitForFile(t, filepath.Join(dir, "done"))
		}()
		// Each next write waits for the preceding callback; the parent stays
		// alive until all writes have been delivered, independent of scheduling.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", `
			(trap 'touch "$1/done"' 0; printf a; i=0; while [ ! -f "$1/a" ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done
			 printf b; i=0; while [ ! -f "$1/b" ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done
			 printf c) &
			i=0; while [ ! -f "$1/done" ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done
		`, "sh", dir)
		var out bytes.Buffer
		err := runBashCommand(cmd, writerFunc(func(p []byte) (int, error) {
			n, err := out.Write(p)
			for _, b := range p {
				if b == 'a' || b == 'b' {
					signalFile(t, filepath.Join(dir, string(b)))
				}
			}
			return n, err
		}))
		if err != nil || out.String() != "abc" {
			t.Fatalf("output %q err %v", out.String(), err)
		}
	})
}

func TestBashDrainCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `printf ready; exec sleep 5`)
	var out bytes.Buffer
	err := runBashCommand(cmd, writerFunc(func(p []byte) (int, error) {
		n, e := out.Write(p)
		if strings.Contains(out.String(), "ready") {
			cancel()
		}
		return n, e
	}))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || out.String() != "ready" {
		t.Fatalf("output %q err %v", out.String(), err)
	}
}

func TestBashDrainContinuousDescendantIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	releasePath := filepath.Join(t.TempDir(), "release")
	releaseWatchdog(t, releasePath)
	cmd := exec.CommandContext(ctx, "sh", "-c", `(i=0; while [ ! -f "$1" ] && [ "$i" -lt 2000 ]; do printf x; sleep .01; i=$((i+1)); done) & exit 0`, "sh", releasePath)
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessTree(cmd) }
	start := time.Now()
	err := runBashCommand(cmd, writerFunc(func(p []byte) (int, error) { return len(p), nil }))
	if !errors.Is(err, ErrBashOutputIncomplete) || time.Since(start) > 4*time.Second {
		t.Fatalf("err %v elapsed %v", err, time.Since(start))
	}
}

func TestBashDrainCancelAfterParentExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// EOF on this independent pipe proves the descendant closed its inherited
	// descriptor; unlike a PID lookup it cannot observe a recycled process.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	releasePath := filepath.Join(t.TempDir(), "release")
	releaseWatchdog(t, releasePath)
	cmd := exec.CommandContext(ctx, "sh", "-c", `(printf R >&3; i=0; while [ ! -f "$1" ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done) & printf ready; exit 0`, "sh", releasePath)
	cmd.ExtraFiles = []*os.File{writer}
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessTree(cmd) }
	done := make(chan error, 1)
	ready := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	go func() {
		done <- runBashCommand(cmd, writerFunc(func(p []byte) (int, error) {
			once.Do(func() { close(ready); <-release })
			return len(p), nil
		}))
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		close(release)
		t.Fatal("no output")
	}
	// A byte proves the witness started. A pending second read proves it still
	// holds the descriptor immediately before cancellation.
	if err := writer.Close(); err != nil {
		close(release)
		t.Fatal(err)
	}
	handshake := make(chan error, 1)
	go func() {
		b := make([]byte, 1)
		_, err := io.ReadFull(reader, b)
		if err == nil && b[0] != 'R' {
			err = errors.New("invalid witness handshake")
		}
		handshake <- err
	}()
	select {
	case err := <-handshake:
		if err != nil {
			close(release)
			t.Fatal(err)
		}
	case <-ctx.Done():
		close(release)
		t.Fatal("witness handshake timed out")
	}
	// The blocked callback prevents reaping while waitid observes actual exit.
	exited := make(chan error, 1)
	go func() { exited <- observeBashExit(cmd.Process.Pid) }()
	select {
	case err := <-exited:
		if err != nil {
			close(release)
			t.Fatal(err)
		}
	case <-ctx.Done():
		close(release)
		t.Fatal("shell did not exit")
	}
	if err := witnessAlive(reader); err != nil {
		close(release)
		t.Fatalf("witness exited before cancellation: %v", err)
	}
	closed := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, reader); closed <- err }()
	cancel()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish drain")
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

func TestBashDrainDelayedDescendantOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `(sleep .3; printf late) & printf early; exit 0`)
	var out bytes.Buffer
	if err := runBashCommand(cmd, &out); err != nil || out.String() != "earlylate" {
		t.Fatalf("output %q err %v", out.String(), err)
	}
}

func TestBashToolIncompleteRetainsOutput(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_, err := bashTool(dir, nil).Execute(ctx, "id", map[string]any{
		"command": `(printf marker; head -c 100000 /dev/zero | tr "\0" x; sleep 4) & exit 0`,
	}, nil)
	if !errors.Is(err, ErrBashOutputIncomplete) || !strings.Contains(err.Error(), "Full output: ") {
		t.Fatalf("missing incomplete error or path: %v", err)
	}
	parts := strings.SplitN(err.Error(), "Full output: ", 2)
	path := strings.SplitN(parts[1], "]", 2)[0]
	data, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.HasPrefix(data, []byte("marker")) {
		t.Fatalf("full output %q: %v", path, readErr)
	}
	os.Remove(path)
	if !strings.Contains(err.Error(), strings.Repeat("x", 100)) {
		t.Fatal("missing captured output")
	}
}

func TestBashDrainActiveBeyondOneSecond(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `(for i in $(seq 1 25); do printf x; sleep .05; done; printf END) & exit 0`)
	var out bytes.Buffer
	err := runBashCommand(cmd, &out)
	if err != nil || !strings.Contains(out.String(), "END") {
		t.Fatalf("output %q, error %v", out.String(), err)
	}
}

func TestBashDrainLimitReportedByOperations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	dir := t.TempDir()
	releaseWatchdog(t, filepath.Join(dir, "release"))
	ops := LocalBashOperations(bashShellConfig)
	code, err := ops.Exec(ctx, `(i=0; while [ ! -f release ] && [ "$i" -lt 2000 ]; do printf x; sleep .01; i=$((i+1)); done) & exit 0`, dir, BashExecOptions{Env: os.Environ()})
	if code == nil || *code != 0 || !errors.Is(err, ErrBashOutputIncomplete) {
		t.Fatalf("code %v error %v", code, err)
	}
}

func TestBashDrainCancellationClassificationAfterExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	releaseWatchdog(t, filepath.Join(dir, "release"))
	ops := LocalBashOperations(bashShellConfig)
	ready, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := ops.Exec(ctx, `echo $$ > pid; (printf x; i=0; while [ ! -f release ] && [ "$i" -lt 2000 ]; do sleep .01; i=$((i+1)); done) & exit 0`, dir, BashExecOptions{Env: os.Environ(), OnData: func([]byte) { once.Do(func() { close(ready); <-release }) }})
		done <- err
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		close(release)
		t.Fatal("no output")
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- observeBashExit(pid) }()
	select {
	case err := <-exited:
		if err != nil {
			close(release)
			t.Fatal(err)
		}
	case <-ctx.Done():
		close(release)
		t.Fatal("shell did not exit")
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrShellAborted) {
			t.Fatalf("error %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not complete")
	}
}

func TestBashIncompletePreservesNonzeroExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	ops := LocalBashOperations(bashShellConfig)
	code, err := ops.Exec(ctx, `(sleep 3) & exit 7`, t.TempDir(), BashExecOptions{Env: os.Environ()})
	if code == nil || *code != 7 || !errors.Is(err, ErrBashOutputIncomplete) {
		t.Fatalf("code %v, error %v", code, err)
	}
	_, err = bashTool(t.TempDir(), nil).Execute(ctx, "id", map[string]any{"command": `(sleep 3) & exit 7`}, nil)
	if !errors.Is(err, ErrBashOutputIncomplete) || !strings.Contains(err.Error(), "Command exited with code 7") {
		t.Fatalf("error %v", err)
	}
}

func TestWitnessAlivePoll(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := witnessAlive(r); err != nil {
		t.Fatalf("open witness: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := witnessAlive(r); err == nil {
		t.Fatal("closed witness reported alive")
	}
}

func TestReleaseWatchdogWithoutStartedProcess(t *testing.T) {
	t.Run("no process", func(t *testing.T) {
		releaseWatchdog(t, filepath.Join(t.TempDir(), "release"))
	})
}
