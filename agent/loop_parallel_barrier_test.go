package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

func barrierWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Once the second intent has entered its admission hook, a sibling failure
// cannot revoke that intent. Both admitted workers must settle before unwind.
func TestParallelFatalDuringAdmissionJoinsAdmittedCalls(t *testing.T) {
	entered := make(chan struct{})
	fail := make(chan struct{})
	failed := make(chan struct{})
	releaseAdmission := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	done := make(chan any, 1)
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(_ context.Context, id string, _ map[string]any, update ToolUpdateFunc) (AgentToolResult, error) {
		if id == "one" {
			<-fail
			update(AgentToolResult{})
			return AgentToolResult{}, nil
		}
		close(secondStarted)
		<-releaseSecond
		return AgentToolResult{}, nil
	}}
	calls := []ai.ToolCall{{ID: "one", Name: "effect", Arguments: map[string]any{}}, {ID: "two", Name: "effect", Arguments: map[string]any{}}}
	go func() {
		defer func() { done <- recover() }()
		executeAdmittedCallsParallelWithPublication(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, nil, calls, AgentLoopConfig{BeforeToolExecute: func(_ context.Context, c BeforeToolCallContext) error {
			if c.ToolCall.ID == "two" {
				close(entered)
				<-releaseAdmission
			}
			return nil
		}}, func(e AgentEvent) error {
			if e.Type == EvToolExecutionUpdate {
				return errors.New("fatal update")
			}
			return nil
		}, func() { close(failed) })
	}()
	defer func() {
		select {
		case <-fail:
		default:
			close(fail)
		}
		select {
		case <-releaseAdmission:
		default:
			close(releaseAdmission)
		}
		select {
		case <-releaseSecond:
		default:
			close(releaseSecond)
		}
	}()
	barrierWait(t, entered, "second admission")
	close(fail)
	barrierWait(t, failed, "fatal update")
	close(releaseAdmission)
	barrierWait(t, secondStarted, "second admitted execution")
	select {
	case p := <-done:
		t.Fatalf("returned before second execution settled: %v", p)
	default:
	}
	close(releaseSecond)
	select {
	case p := <-done:
		if _, ok := p.(emitPanic); !ok {
			t.Fatalf("panic = %v, want emitPanic", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not unwind")
	}
}

// Replay is part of admission and may depend on a previously admitted worker's
// finalization. Holding a batch-wide lock across either callback deadlocks.
func TestParallelReplayCanWaitForSiblingFinalization(t *testing.T) {
	finalized := make(chan struct{})
	done := make(chan any, 1)
	calls := []ai.ToolCall{{ID: "one", Name: "effect", Arguments: map[string]any{}}, {ID: "two", Name: "effect", Arguments: map[string]any{}}}
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
		return AgentToolResult{}, nil
	}}
	go func() {
		defer func() { done <- recover() }()
		executeAdmittedCallsParallel(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, nil, calls, AgentLoopConfig{
			BeforeToolExecute: func(context.Context, BeforeToolCallContext) error { return nil },
			ReplayToolResult: func(_ context.Context, c BeforeToolCallContext) (*ai.ToolResultMessage, error) {
				if c.ToolCall.ID == "two" {
					<-finalized
				}
				return nil, nil
			},
			AfterToolCall: func(_ context.Context, c AfterToolCallContext) *AfterToolCallResult {
				if c.ToolCall.ID == "one" {
					close(finalized)
				}
				return nil
			},
		}, func(AgentEvent) error { return nil })
	}()
	select {
	case p := <-done:
		if p != nil {
			t.Fatalf("panic: %v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replay blocked sibling finalization")
	}
}

func TestParallelPublishedFatalPreventsNextAdmission(t *testing.T) {
	published := make(chan struct{})
	done := make(chan any, 1)
	admitted := make(chan string, 2)
	calls := []ai.ToolCall{{ID: "one", Name: "effect", Arguments: map[string]any{}}, {ID: "two", Name: "effect", Arguments: map[string]any{}}}
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(_ context.Context, _ string, _ map[string]any, update ToolUpdateFunc) (AgentToolResult, error) {
		update(AgentToolResult{})
		return AgentToolResult{}, nil
	}}
	go func() {
		defer func() { done <- recover() }()
		executeAdmittedCallsParallelWithPublication(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, nil, calls, AgentLoopConfig{
			BeforeToolExecute: func(_ context.Context, c BeforeToolCallContext) error { admitted <- c.ToolCall.ID; return nil },
			HasSteeringMessages: func() bool {
				if len(admitted) > 0 {
					<-published
				}
				return false
			},
		}, func(e AgentEvent) error {
			if e.Type == EvToolExecutionUpdate {
				return errors.New("fatal update")
			}
			return nil
		}, func() { close(published) })
	}()
	select {
	case p := <-done:
		if _, ok := p.(emitPanic); !ok {
			t.Fatalf("panic = %v, want emitPanic", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not unwind")
	}
	if len(admitted) != 1 || <-admitted != "one" {
		t.Fatal("admission continued after fatal publication")
	}
}
