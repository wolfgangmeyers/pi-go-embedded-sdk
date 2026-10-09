package coding

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

func TestCodingSessionMultiToolInterruptedBySteering(t *testing.T) {
	var tool1Called, tool2Called atomic.Bool
	tool1Started := make(chan struct{})
	allowTool1Finish := make(chan struct{})

	tool1 := agent.AgentTool{
		Name:        "tool1",
		Description: "Tool 1",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			tool1Called.Store(true)
			close(tool1Started)
			<-allowTool1Finish
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "tool1 completed"}}}, nil
		},
	}

	tool2 := agent.AgentTool{
		Name:        "tool2",
		Description: "Tool 2",
		Parameters:  ai.Object(),
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
			tool2Called.Store(true)
			return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "tool2 completed"}}}, nil
		},
	}

	var requestCount int
	var sess *Session
	fakeModel := &ai.Model{
		ID:        "mock-model",
		Provider:  "mock-provider",
		Api:       ai.APIOpenAICompletions,
		MaxTokens: 4096,
	}

	streamFn := func(ctx context.Context, m *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		s := ai.NewAssistantMessageEventStream()
		requestCount++
		currentReq := requestCount
		go func() {
			var msg *ai.AssistantMessage
			if currentReq == 1 {
				// Turn 1: Return multiple tool calls
				msg = &ai.AssistantMessage{
					Content: ai.ContentList{
						ai.ToolCall{ID: "call_tool1", Name: "tool1", Arguments: map[string]any{}},
						ai.ToolCall{ID: "call_tool2", Name: "tool2", Arguments: map[string]any{}},
					},
					Api:        m.Api,
					Provider:   m.Provider,
					Model:      m.ID,
					StopReason: ai.StopToolUse,
					Timestamp:  1,
				}
			} else {
				// Turn 2: Assistant responds to the steered message
				msg = &ai.AssistantMessage{
					Content:    ai.ContentList{ai.TextContent{Text: "received your steering message immediately"}},
					Api:        m.Api,
					Provider:   m.Provider,
					Model:      m.ID,
					StopReason: ai.StopStop,
					Timestamp:  2,
				}
			}
			s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: msg.StopReason, Message: msg})
			s.End()
		}()
		return s
	}

	sess = NewSession(SessionOptions{
		Model:    fakeModel,
		Cwd:      t.TempDir(),
		Tools:    []agent.AgentTool{tool1, tool2},
		StreamFn: streamFn,
	})

	go func() {
		<-tool1Started
		// Steer while tool1 is running
		sess.Steer(ai.NewUserText("stop now and listen", 0))
		if !sess.HasSteeringMessages() {
			t.Errorf("expected HasSteeringMessages() to be true after Steer()")
		}
		close(allowTool1Finish)
	}()

	res, err := sess.Run(context.Background(), "run both tools")
	if err != nil {
		t.Fatalf("unexpected error from Run: %v", err)
	}

	if !tool1Called.Load() {
		t.Fatal("expected tool1 to be executed")
	}
	if tool2Called.Load() {
		t.Fatal("tool2 MUST NOT be executed because steering arrived mid-batch!")
	}

	if !strings.Contains(res.Text, "received your steering message immediately") {
		t.Fatalf("expected final text to be response to steering, got: %s", res.Text)
	}
}
