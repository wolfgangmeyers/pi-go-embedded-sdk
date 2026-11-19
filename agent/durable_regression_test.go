package agent

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

func TestNestedAdmissionAbortPropagatesThroughToolExecution(t *testing.T) {
	inner := AgentTool{Name: "inner", Parameters: &ai.Schema{Type: "object"}, Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
		t.Fatal("inner executed")
		return AgentToolResult{}, nil
	}}
	outer := AgentTool{Name: "outer", Parameters: &ai.Schema{Type: "object"}, Execute: func(ctx context.Context, _ string, _ map[string]any, _ ToolUpdateFunc) (AgentToolResult, error) {
		RunToolCall(ctx, ai.ToolCall{ID: "i", Name: "inner", Arguments: map[string]any{}}, RunToolCallOptions{Tools: []AgentTool{inner}, ToolCallHooks: ToolCallHooks{BeforeToolExecute: func(context.Context, BeforeToolCallContext) error { return errors.New("admission failed") }}})
		return AgentToolResult{}, nil
	}}
	defer func() {
		if _, ok := recover().(toolAdmissionAbort); !ok {
			t.Error("nested admission failure was converted to tool result")
		}
	}()
	RunToolCall(context.Background(), ai.ToolCall{ID: "o", Name: "outer", Arguments: map[string]any{}}, RunToolCallOptions{Tools: []AgentTool{outer}})
}

func TestDurableAdmissionDoesNotRunAheadOfSteering(t *testing.T) {
	steering := false
	var admitted, executed []string
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(_ context.Context, id string, _ map[string]any, _ ToolUpdateFunc) (AgentToolResult, error) {
		executed = append(executed, id)
		steering = true
		return AgentToolResult{}, nil
	}}
	calls := []ai.ToolCall{{ID: "one", Name: "effect", Arguments: map[string]any{}}, {ID: "two", Name: "effect", Arguments: map[string]any{}}}
	msg := &ai.AssistantMessage{Content: ai.ContentList{calls[0], calls[1]}}
	config := AgentLoopConfig{ToolExecution: ToolSequential, BeforeToolExecute: func(_ context.Context, c BeforeToolCallContext) error {
		admitted = append(admitted, c.ToolCall.ID)
		return nil
	}, HasSteeringMessages: func() bool { return steering }}
	executeToolCalls(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, msg, config, func(AgentEvent) error { return nil })
	if !reflect.DeepEqual(admitted, []string{"one", "two"}) || !reflect.DeepEqual(executed, []string{"one", "two"}) {
		t.Fatalf("admitted=%v executed=%v", admitted, executed)
	}
}

func TestParallelAdmissionExecutesConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(_ context.Context, id string, _ map[string]any, _ ToolUpdateFunc) (AgentToolResult, error) {
		started <- id
		<-release
		return AgentToolResult{}, nil
	}}
	calls := []ai.ToolCall{{ID: "one", Name: "effect", Arguments: map[string]any{}}, {ID: "two", Name: "effect", Arguments: map[string]any{}}}
	msg := &ai.AssistantMessage{Content: ai.ContentList{calls[0], calls[1]}}
	var admitted []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		executeToolCalls(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, msg, AgentLoopConfig{BeforeToolExecute: func(_ context.Context, c BeforeToolCallContext) error {
			admitted = append(admitted, c.ToolCall.ID)
			return nil
		}}, func(AgentEvent) error { return nil })
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			<-done
			t.Fatal("tool executions did not overlap")
		}
	}
	close(release)
	<-done
	if !reflect.DeepEqual(admitted, []string{"one", "two"}) {
		t.Fatalf("admitted=%v", admitted)
	}
}

func TestParallelSteeringSkipsOnlyUnadmittedCalls(t *testing.T) {
	var steering atomic.Bool
	var admitted []string
	executed := make(chan string, 2)
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(_ context.Context, id string, _ map[string]any, _ ToolUpdateFunc) (AgentToolResult, error) {
		executed <- id
		return AgentToolResult{}, nil
	}}
	calls := []ai.ToolCall{{ID: "one", Name: "effect", Arguments: map[string]any{}}, {ID: "two", Name: "effect", Arguments: map[string]any{}}}
	msg := &ai.AssistantMessage{Content: ai.ContentList{calls[0], calls[1]}}
	batch := executeToolCalls(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, msg, AgentLoopConfig{
		HasSteeringMessages: steering.Load,
		BeforeToolExecute: func(_ context.Context, c BeforeToolCallContext) error {
			admitted = append(admitted, c.ToolCall.ID)
			steering.Store(true)
			return nil
		},
	}, func(AgentEvent) error { return nil })
	if !reflect.DeepEqual(admitted, []string{"one", "two"}) || len(executed) != 2 {
		t.Fatalf("admitted=%v executed count=%d", admitted, len(executed))
	}
	if len(batch.messages) != 2 || batch.messages[0].IsError || batch.messages[1].IsError {
		t.Fatalf("results=%+v", batch.messages)
	}
}

func TestParallelAdmissionFailureJoinsAdmittedCall(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan any, 1)
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(_ context.Context, _ string, _ map[string]any, _ ToolUpdateFunc) (AgentToolResult, error) {
		close(started)
		<-release
		return AgentToolResult{}, nil
	}}
	calls := []ai.ToolCall{{ID: "one", Name: "effect", Arguments: map[string]any{}}, {ID: "two", Name: "effect", Arguments: map[string]any{}}}
	msg := &ai.AssistantMessage{Content: ai.ContentList{calls[0], calls[1]}}
	failed := make(chan struct{})
	go func() {
		defer func() { done <- recover() }()
		executeToolCalls(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, msg, AgentLoopConfig{BeforeToolExecute: func(_ context.Context, c BeforeToolCallContext) error {
			if c.ToolCall.ID == "two" {
				<-started
				close(failed)
				return errors.New("failed")
			}
			return nil
		}}, func(AgentEvent) error { return nil })
	}()
	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("second admission did not run")
	}
	select {
	case <-done:
		close(release)
		t.Fatal("returned before admitted call settled")
	default:
	}
	close(release)
	select {
	case r := <-done:
		if _, ok := r.(toolAdmissionAbort); !ok {
			t.Fatalf("panic=%v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not finish")
	}
}

func TestParallelAdmissionHooksAllowWorkerUpdatesAndFinalization(t *testing.T) {
	updated := make(chan struct{})
	finalized := make(chan struct{})
	calls := []ai.ToolCall{{ID: "one", Name: "effect", Arguments: map[string]any{}}, {ID: "two", Name: "effect", Arguments: map[string]any{}}}
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(_ context.Context, id string, _ map[string]any, update ToolUpdateFunc) (AgentToolResult, error) {
		if id == "one" {
			update(AgentToolResult{})
		}
		return AgentToolResult{}, nil
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		executeToolCalls(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, &ai.AssistantMessage{Content: ai.ContentList{calls[0], calls[1]}}, AgentLoopConfig{
			BeforeToolExecute: func(_ context.Context, c BeforeToolCallContext) error {
				if c.ToolCall.ID == "two" {
					<-updated
					<-finalized
				}
				return nil
			},
			AfterToolCall: func(_ context.Context, c AfterToolCallContext) *AfterToolCallResult {
				if c.ToolCall.ID == "one" {
					close(finalized)
				}
				return nil
			},
		}, func(e AgentEvent) error {
			if e.Type == EvToolExecutionUpdate {
				close(updated)
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("admission blocked worker callbacks")
	}
}
