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

func TestBashDrainSlowCallbackPreservesBufferedTail(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "gate")
	finished := filepath.Join(dir, "finished")
	cmd := exec.Command("sh", "-c", `printf head; while [ ! -f "$1" ]; do sleep .01; done; printf tail; touch "$2"`, "sh", gate, finished)
	var output bytes.Buffer
	first := true
	err := runBashCommand(cmd, writerFunc(func(p []byte) (int, error) {
		output.Write(p)
		if first {
			first = false
			if err := os.WriteFile(gate, nil, 0600); err != nil {
				t.Error(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(finished); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Error("child did not finish")
					break
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(3 * bashExitStdioGrace)
		}
		return len(p), nil
	}))
	if err != nil || output.String() != "headtail" {
		t.Fatalf("output %q, err %v", output.String(), err)
	}
}

func TestBashDrainDescendantIdleAndOutput(t *testing.T) {
	for _, tc := range []struct{ name, script, want string }{
		{"quiet", `(sleep 2) & printf done`, "done"},
		{"active", `(sleep .06; printf a; sleep .06; printf b; sleep .06; printf c) & printf start`, "startabc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			start := time.Now()
			err := runBashCommand(exec.Command("sh", "-c", tc.script), &out)
			if err != nil || out.String() != tc.want {
				t.Fatalf("output %q err %v", out.String(), err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("quiet pipe was not released")
			}
		})
	}
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
