//go:build linux || darwin

package coding

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestBashDrainSlowCallbackPreservesBufferedTail(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	exited := filepath.Join(dir, "exited")
	release := filepath.Join(dir, "release")
	defer signalFile(t, release)
	// The descendant retains the pipe while the callback is blocked.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `
		(while [ ! -f "$3" ]; do sleep .01; done) &
		printf head; while [ ! -f "$1" ]; do sleep .01; done; printf tail; touch "$2"
	`, "sh", gate, exited, release)
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
		release := filepath.Join(t.TempDir(), "release")
		defer signalFile(t, release)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		var out bytes.Buffer
		go func() {
			done <- runBashCommand(exec.CommandContext(ctx, "sh", "-c", `(while [ ! -f "$1" ]; do sleep .01; done) & printf done`, "sh", release), &out)
		}()
		select {
		case err := <-done:
			if err != nil || out.String() != "done" {
				t.Fatalf("output %q err %v", out.String(), err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("quiet pipe was not released")
		}
	})
	t.Run("active", func(t *testing.T) {
		dir := t.TempDir()
		defer signalFile(t, filepath.Join(dir, "a"))
		defer signalFile(t, filepath.Join(dir, "b"))
		// Each next write waits for the preceding callback; the parent stays
		// alive until all writes have been delivered, independent of scheduling.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", `
			(printf a; while [ ! -f "$1/a" ]; do sleep .01; done
			 printf b; while [ ! -f "$1/b" ]; do sleep .01; done
			 printf c; touch "$1/done") &
			while [ ! -f "$1/done" ]; do sleep .01; done
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
	cmd := exec.CommandContext(ctx, "sh", "-c", `(while :; do printf x; done) & exit 0`)
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessTree(cmd) }
	start := time.Now()
	err := runBashCommand(cmd, writerFunc(func(p []byte) (int, error) { return len(p), nil }))
	if !errors.Is(err, ErrBashOutputIncomplete) || time.Since(start) > 4*time.Second {
		t.Fatalf("err %v elapsed %v", err, time.Since(start))
	}
}

func TestBashDrainCancelAfterParentExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `(while :; do printf x; done) & exit 0`)
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessTree(cmd) }
	done := make(chan error, 1)
	ready := make(chan struct{})
	var once sync.Once
	go func() {
		done <- runBashCommand(cmd, writerFunc(func(p []byte) (int, error) { once.Do(func() { close(ready) }); return len(p), nil }))
	}()
	<-ready
	// The observer leaves the shell waitable; waitid observes its actual exit.
	if err := observeBashExit(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not finish drain")
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
	ops := LocalBashOperations(bashShellConfig)
	code, err := ops.Exec(ctx, `(while :; do printf x; done) & exit 0`, t.TempDir(), BashExecOptions{Env: os.Environ()})
	if code != nil || !errors.Is(err, ErrBashOutputIncomplete) {
		t.Fatalf("code %v error %v", code, err)
	}
}

func TestBashDrainCancellationClassificationAfterExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ops := LocalBashOperations(bashShellConfig)
	ready := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := ops.Exec(ctx, `(while :; do printf x; done) & exit 0`, t.TempDir(), BashExecOptions{Env: os.Environ(), OnData: func([]byte) { once.Do(func() { close(ready) }) }})
		done <- err
	}()
	<-ready
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrShellAborted) {
			t.Fatalf("error %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("cancellation did not complete")
	}
}
