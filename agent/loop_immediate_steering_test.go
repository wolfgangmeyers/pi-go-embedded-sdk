package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func assistantWithToolList(calls ...ai.ToolCall) *ai.AssistantMessage {
	content := make(ai.ContentList, len(calls))
	for i, c := range calls {
		content[i] = c
	}
	return &ai.AssistantMessage{
		Content:    content,
		StopReason: ai.StopToolUse,
		Model:      "faux",
	}
}

func trText(tr ai.ToolResultMessage) string {
	var sb strings.Builder
	for _, c := range tr.Content {
		if tc, ok := c.(ai.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func asstText(msg *ai.AssistantMessage) string {
	var sb strings.Builder
	for _, c := range msg.Content {
		if tc, ok := c.(ai.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestSteeringMidToolBatchSkipsRemainingToolsAndInvokesModel(t *testing.T) {
	var toolACalled, toolBCalled, toolCCalled atomic.Bool
	var agentRef *Agent

	toolA := AgentTool{
		Name:          "toolA",
		Description:   "Tool A",
		Parameters:    ai.Object(),
		ExecutionMode: ToolSequential,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			toolACalled.Store(true)
			// Steer while toolA is running
			agentRef.Steer(ai.NewUserText("stop and respond to me", 0))
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "resultA"}}}, nil
		},
	}
	toolB := AgentTool{
		Name:          "toolB",
		Description:   "Tool B",
		Parameters:    ai.Object(),
		ExecutionMode: ToolSequential,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			toolBCalled.Store(true)
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "resultB"}}}, nil
		},
	}
	toolC := AgentTool{
		Name:          "toolC",
		Description:   "Tool C",
		Parameters:    ai.Object(),
		ExecutionMode: ToolSequential,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			toolCCalled.Store(true)
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "resultC"}}}, nil
		},
	}

	turn1 := assistantWithToolList(
		ai.ToolCall{ID: "call_a", Name: "toolA", Arguments: map[string]any{}},
		ai.ToolCall{ID: "call_b", Name: "toolB", Arguments: map[string]any{}},
		ai.ToolCall{ID: "call_c", Name: "toolC", Arguments: map[string]any{}},
	)
	turn2 := textMessage("I stopped and received your message: stop and respond to me")

	var requests []ai.TranscriptContext
	scripted := scriptedStream(turn1, turn2)

	streamFn := func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		requests = append(requests, req)
		return scripted(ctx, model, req, opts)
	}

	a := NewAgent(AgentOptions{
		InitialState: &AgentState{
			Model: testModel,
			Tools: []AgentTool{toolA, toolB, toolC},
		},
		StreamFn: streamFn,
	})
	agentRef = a

	if err := a.Prompt(context.Background(), "initial task"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !toolACalled.Load() {
		t.Fatal("expected toolA to be called")
	}
	if toolBCalled.Load() {
		t.Fatal("toolB should NOT have been called because steering arrived")
	}
	if toolCCalled.Load() {
		t.Fatal("toolC should NOT have been called because steering arrived")
	}

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}

	// In request 2, verify transcript order:
	// tool results for A, B, C, followed by the user steering message
	req2Messages := requests[1].Messages
	if len(req2Messages) < 5 {
		t.Fatalf("expected at least 5 messages in req2, got %d", len(req2Messages))
	}

	var resA, resB, resC *ai.ToolResultMessage
	var steeringUser *ai.UserMessage
	for _, m := range req2Messages {
		if tr, ok := m.(ai.ToolResultMessage); ok {
			switch tr.ToolCallID {
			case "call_a":
				copyTR := tr
				resA = &copyTR
			case "call_b":
				copyTR := tr
				resB = &copyTR
			case "call_c":
				copyTR := tr
				resC = &copyTR
			}
		} else if u, ok := m.(ai.UserMessage); ok {
			if strings.Contains(userText(u), "stop and respond to me") {
				copyU := u
				steeringUser = &copyU
			}
		}
	}

	if resA == nil || resA.IsError {
		t.Fatalf("expected resA to be non-error, got: %+v", resA)
	}
	if resB == nil || !resB.IsError || !strings.Contains(trText(*resB), "interrupted by user steering message") {
		t.Fatalf("expected resB to be skipped with steering message, got: %+v", resB)
	}
	if resC == nil || !resC.IsError || !strings.Contains(trText(*resC), "interrupted by user steering message") {
		t.Fatalf("expected resC to be skipped with steering message, got: %+v", resC)
	}
	if steeringUser == nil {
		t.Fatal("expected steering user message to be present in request 2")
	}

	// Verify the final message in agent state is the assistant's response to the steering message
	finalMsg, ok := asAssistant(a.State().Messages[len(a.State().Messages)-1])
	if !ok || !strings.Contains(asstText(finalMsg), "I stopped and received your message") {
		t.Fatalf("expected assistant response to steering, got: %+v", a.State().Messages[len(a.State().Messages)-1])
	}
}

func TestSteeringBeforeToolExecutionSkipsAllToolsAndInvokesModel(t *testing.T) {
	var toolACalled, toolBCalled atomic.Bool

	toolA := AgentTool{
		Name:          "toolA",
		Description:   "Tool A",
		Parameters:    ai.Object(),
		ExecutionMode: ToolSequential,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			toolACalled.Store(true)
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "resultA"}}}, nil
		},
	}
	toolB := AgentTool{
		Name:          "toolB",
		Description:   "Tool B",
		Parameters:    ai.Object(),
		ExecutionMode: ToolSequential,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			toolBCalled.Store(true)
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "resultB"}}}, nil
		},
	}

	turn1 := assistantWithToolList(
		ai.ToolCall{ID: "call_a", Name: "toolA", Arguments: map[string]any{}},
		ai.ToolCall{ID: "call_b", Name: "toolB", Arguments: map[string]any{}},
	)
	turn2 := textMessage("Acknowledged steering without running tools")

	var requests []ai.TranscriptContext
	scripted := scriptedStream(turn1, turn2)

	var a *Agent
	streamFn := func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		requests = append(requests, req)
		if len(requests) == 1 {
			// Steer while the first response is being streamed, before any tools execute
			a.Steer(ai.NewUserText("immediate steering before tools", 0))
		}
		return scripted(ctx, model, req, opts)
	}

	a = NewAgent(AgentOptions{
		InitialState: &AgentState{
			Model: testModel,
			Tools: []AgentTool{toolA, toolB},
		},
		StreamFn: streamFn,
	})

	if err := a.Prompt(context.Background(), "initial task"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if toolACalled.Load() {
		t.Fatal("toolA should NOT have been called because steering arrived before execution")
	}
	if toolBCalled.Load() {
		t.Fatal("toolB should NOT have been called because steering arrived before execution")
	}

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}

	// Verify both tool results are skipped error results
	req2Messages := requests[1].Messages
	var resA, resB *ai.ToolResultMessage
	for _, m := range req2Messages {
		if tr, ok := m.(ai.ToolResultMessage); ok {
			switch tr.ToolCallID {
			case "call_a":
				copyTR := tr
				resA = &copyTR
			case "call_b":
				copyTR := tr
				resB = &copyTR
			}
		}
	}

	if resA == nil || !resA.IsError || !strings.Contains(trText(*resA), "interrupted by user steering message") {
		t.Fatalf("expected resA to be skipped, got: %+v", resA)
	}
	if resB == nil || !resB.IsError || !strings.Contains(trText(*resB), "interrupted by user steering message") {
		t.Fatalf("expected resB to be skipped, got: %+v", resB)
	}
}

func TestSteeredMessageInvokingToolDoesNotAbortItself(t *testing.T) {
	var subagentCalled atomic.Bool

	subagentTool := AgentTool{
		Name:          "subagent",
		Description:   "Launch subagent",
		Parameters:    ai.Object(),
		ExecutionMode: ToolSequential,
		Execute: func(ctx context.Context, id string, params map[string]any, onUpdate ToolUpdateFunc) (AgentToolResult, error) {
			subagentCalled.Store(true)
			return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "subagent started"}}}, nil
		},
	}

	turn1 := textMessage("Initial work before steering")
	turn2 := assistantWithToolList(ai.ToolCall{ID: "call_subagent", Name: "subagent", Arguments: map[string]any{"task": "fix UI"}})
	turn3 := textMessage("Subagent finished and work is complete")

	var requests []ai.TranscriptContext
	scripted := scriptedStream(turn1, turn2, turn3)

	var a *Agent
	streamFn := func(ctx context.Context, model *ai.Model, req ai.TranscriptContext, opts *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		requests = append(requests, req)
		if len(requests) == 1 {
			// User steers mid-stream with subagent guidance (simulate duplicate steer as well)
			a.Steer(ai.NewUserText("Important: Subagent Guidance: Use subagents for your work", 0))
			a.Steer(ai.NewUserText("Important: Subagent Guidance: Use subagents for your work", 0))
		}
		return scripted(ctx, model, req, opts)
	}

	a = NewAgent(AgentOptions{
		InitialState: &AgentState{
			Model: testModel,
			Tools: []AgentTool{subagentTool},
		},
		StreamFn: streamFn,
	})

	if err := a.Prompt(context.Background(), "initial task"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !subagentCalled.Load() {
		t.Fatal("expected subagent tool to be executed, but it was skipped/aborted as interrupted!")
	}
}

