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
	// The descendant retains the pipe and confirms the actual parent exit (and
	// reap), rather than merely observing a marker written before exit.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `
		parent=$$
		(while kill -0 "$parent" 2>/dev/null; do sleep .01; done
		 touch "$2"; while [ ! -f "$3" ]; do sleep .01; done) &
		printf head; while [ ! -f "$1" ]; do sleep .01; done; printf tail
	`, "sh", gate, exited, release)
	var output bytes.Buffer
	first := true
	err := runBashCommand(cmd, writerFunc(func(p []byte) (int, error) {
		output.Write(p)
		if first {
			first = false
			signalFile(t, gate)
			if waitForFile(t, exited) {
				// Make the callback exceed the post-exit idle window while the
				// tail is already buffered and the descendant holds the pipe.
				time.Sleep(3 * bashExitStdioGrace)
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
