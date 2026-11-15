package agent

import (
	"context"
	"errors"
	"testing"

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
	config := AgentLoopConfig{BeforeToolExecute: func(_ context.Context, c BeforeToolCallContext) error {
		admitted = append(admitted, c.ToolCall.ID)
		return nil
	}, HasSteeringMessages: func() bool { return steering }}
	executeToolCalls(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, msg, config, func(AgentEvent) error { return nil })
	if len(admitted) != 1 || len(executed) != 1 || admitted[0] != "one" || executed[0] != "one" {
		t.Fatalf("admitted=%v executed=%v", admitted, executed)
	}
}
