package agent

import (
	"context"
	"github.com/sky-valley/pi/ai"
	"strings"
	"testing"
)

func TestIssue45UnknownToolEarlyResultIsSafe(t *testing.T) {
	got := prepareToolCall(context.Background(), &AgentContext{}, nil, ai.ToolCall{Name: "SECRET_TOKEN\nINJECT"}, AgentLoopConfig{})
	if got.immediate == nil || !got.immediate.isError {
		t.Fatal("expected immediate rejection")
	}
	text := got.immediate.result.Content[0].(ai.TextContent).Text
	if strings.Contains(text, "SECRET_TOKEN") || strings.Contains(text, "\n") {
		t.Fatalf("unsafe result: %q", text)
	}
}

func TestIssue45InvalidArgumentsShowSafeDetailBeforeAdmission(t *testing.T) {
	admitted := false
	tool := AgentTool{Name: "read", Parameters: ai.Object(ai.Prop("path", ai.String()))}
	config := AgentLoopConfig{BeforeToolExecute: func(context.Context, BeforeToolCallContext) error { admitted = true; return nil }}
	got := prepareToolCall(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, nil, ai.ToolCall{Name: "read", Arguments: map[string]any{}}, config)
	if got.immediate == nil || !got.immediate.isError || admitted {
		t.Fatal("rejection must precede admission")
	}
	text := got.immediate.result.Content[0].(ai.TextContent).Text
	if !strings.Contains(text, "path") {
		t.Fatalf("missing field diagnostic: %s", text)
	}
}

func TestValidationRejectionHookRunsBeforeAdmission(t *testing.T) {
	for _, args := range []map[string]any{{}, {"command": map[string]any{"secret": "x"}}} {
		admitted, executed := false, false
		tool := AgentTool{Name: "bash", Parameters: ai.Object(ai.Prop("command", ai.String())), ValidationError: func(args map[string]any) string {
			if len(args) == 0 {
				return "bash missing command"
			}
			return "bash command must be string"
		}, Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
			executed = true
			return AgentToolResult{}, nil
		}}
		got := prepareToolCall(context.Background(), &AgentContext{Tools: []AgentTool{tool}}, nil, ai.ToolCall{Name: "bash", Arguments: args}, AgentLoopConfig{BeforeToolExecute: func(context.Context, BeforeToolCallContext) error { admitted = true; return nil }})
		if got.immediate == nil || !got.immediate.isError || admitted || executed {
			t.Fatal("expected rejection before admission")
		}
		text := got.immediate.result.Content[0].(ai.TextContent).Text
		if !strings.Contains(text, "bash") || !strings.Contains(text, "command") {
			t.Fatalf("bad diagnostic %q", text)
		}
	}
}
