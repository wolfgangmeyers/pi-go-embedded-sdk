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
