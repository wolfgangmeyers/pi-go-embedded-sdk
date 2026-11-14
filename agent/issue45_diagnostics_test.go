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
