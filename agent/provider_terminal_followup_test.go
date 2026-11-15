package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestProviderTerminalReasonsAndStarts(t *testing.T) {
	for _, tt := range []struct {
		name         string
		typ          ai.EventType
		reason, stop ai.StopReason
		want         ai.StopReason
		diagnostic   bool
	}{
		{"valid", ai.EventDone, ai.StopStop, ai.StopStop, ai.StopStop, false},
		{"tool", ai.EventDone, ai.StopToolUse, ai.StopToolUse, ai.StopToolUse, false},
		{"length", ai.EventDone, ai.StopLength, ai.StopLength, ai.StopLength, false},
		{"omitted event", ai.EventDone, "", ai.StopToolUse, ai.StopToolUse, false},
		{"omitted message", ai.EventDone, ai.StopToolUse, "", ai.StopToolUse, false},
		{"contradiction", ai.EventDone, ai.StopStop, ai.StopToolUse, ai.StopError, true},
		{"error contradiction", ai.EventError, ai.StopError, ai.StopToolUse, ai.StopError, true},
		{"empty error", ai.EventError, "", "", ai.StopError, true},
		{"cancelled", ai.EventError, ai.StopAborted, ai.StopAborted, ai.StopAborted, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ac := &AgentContext{Messages: []AgentMessage{ai.NewUserText("hello", 1)}}
			var events []AgentEvent
			fn := func(context.Context, *ai.Model, ai.TranscriptContext, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				s := ai.NewAssistantMessageEventStream()
				p := &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "partial"}}}
				s.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, Partial: p})
				s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: p})
				s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: p})
				m := &ai.AssistantMessage{StopReason: tt.stop, Content: ai.ContentList{ai.ToolCall{ID: "1", Name: "danger"}}}
				ev := ai.AssistantMessageEvent{Type: tt.typ, Reason: tt.reason, Message: m, Error: m}
				s.Push(ev)
				return s
			}
			got := streamAssistantResponse(context.Background(), ac, AgentLoopConfig{Model: &ai.Model{}}, func(e AgentEvent) error { events = append(events, e); return nil }, fn)
			if got.StopReason != tt.want || (tt.diagnostic && strings.TrimSpace(got.ErrorMessage) == "") {
				t.Fatalf("result: %+v", got)
			}
			if len(ac.Messages) != 2 || ac.Messages[1] != got {
				t.Fatalf("context: %#v", ac.Messages)
			}
			starts := 0
			for _, e := range events {
				if e.Type == EvMessageStart {
					starts++
				}
			}
			if starts != 1 {
				t.Fatalf("starts = %d", starts)
			}
		})
	}
}

func TestProviderFailurePreservesPointerSnapshots(t *testing.T) {
	text := &ai.TextContent{Text: "text"}
	thinking := &ai.ThinkingContent{Thinking: "thought"}
	ac := &AgentContext{}
	fn := func(context.Context, *ai.Model, ai.TranscriptContext, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
		s := ai.NewAssistantMessageEventStream()
		s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: &ai.AssistantMessage{Content: ai.ContentList{text, thinking, (*ai.TextContent)(nil), ai.ToolCall{Name: "danger"}}}})
		s.End()
		return s
	}
	got := streamAssistantResponse(context.Background(), ac, AgentLoopConfig{Model: &ai.Model{}}, func(AgentEvent) error { return nil }, fn)
	text.Text = "changed"
	thinking.Thinking = "changed"
	if len(got.Content) != 2 || got.Content[0].(*ai.TextContent).Text != "text" || got.Content[1].(*ai.ThinkingContent).Thinking != "thought" {
		t.Fatalf("content: %#v", got.Content)
	}
}

func TestContradictoryTerminalDoesNotExecuteTools(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelFirst {
			cancel()
		}
		runs := 0
		tool := AgentTool{Name: "danger", Parameters: ai.Object(), Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
			runs++
			return AgentToolResult{}, nil
		}}
		fn := func(context.Context, *ai.Model, ai.TranscriptContext, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			s := ai.NewAssistantMessageEventStream()
			s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: ai.StopStop, Message: &ai.AssistantMessage{StopReason: ai.StopToolUse, Content: ai.ContentList{ai.ToolCall{ID: "1", Name: "danger", Arguments: map[string]any{}}}}})
			return s
		}
		stream := AgentLoop(ctx, []AgentMessage{ai.NewUserText("hello", 1)}, AgentContext{Tools: []AgentTool{tool}}, AgentLoopConfig{Model: &ai.Model{}}, fn)
		for range stream.Events() {
		}
		msgs := stream.Result()
		m := msgs[len(msgs)-1].(*ai.AssistantMessage)
		want := ai.StopError
		if cancelFirst {
			want = ai.StopAborted
		}
		if runs != 0 || m.StopReason != want || m.ErrorMessage == "" {
			t.Fatalf("cancel=%v runs=%d message=%+v", cancelFirst, runs, m)
		}
		cancel()
	}
}

func TestTerminalReasonControlsFullLoop(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		event, message, want ai.StopReason
		runs                 int
		resultError          bool
		followup             bool
	}{
		{"length", ai.StopLength, "", ai.StopLength, 0, true, true},
		{"stop compatibility", ai.StopStop, "", ai.StopStop, 1, false, true},
		{"tool use", ai.StopToolUse, "", ai.StopToolUse, 1, false, true},
		{"conflicting", ai.StopStop, ai.StopToolUse, ai.StopError, 0, false, false},
		{"both empty", "", "", "", 1, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runs, calls := 0, 0
			tool := AgentTool{Name: "danger", Parameters: ai.Object(), Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
				runs++
				return AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "ok"}}}, nil
			}}
			original := &ai.AssistantMessage{StopReason: tt.message, Content: ai.ContentList{ai.ToolCall{ID: "1", Name: "danger", Arguments: map[string]any{}}}}
			fn := func(_ context.Context, _ *ai.Model, _ ai.TranscriptContext, _ *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				calls++
				s := ai.NewAssistantMessageEventStream()
				if calls == 1 {
					s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: tt.event, Message: original})
				} else {
					s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Reason: ai.StopStop, Message: &ai.AssistantMessage{StopReason: ai.StopStop}})
				}
				return s
			}
			stream := AgentLoop(context.Background(), []AgentMessage{ai.NewUserText("hello", 1)}, AgentContext{Tools: []AgentTool{tool}}, AgentLoopConfig{Model: &ai.Model{}}, fn)
			for range stream.Events() {
			}
			msgs := stream.Result()
			var first *ai.AssistantMessage
			var results []ai.ToolResultMessage
			for _, msg := range msgs {
				switch m := msg.(type) {
				case *ai.AssistantMessage:
					if first == nil {
						first = m
					}
				case ai.ToolResultMessage:
					results = append(results, m)
				}
			}
			if first == nil || first.StopReason != tt.want || runs != tt.runs || (calls == 2) != tt.followup || original.StopReason != tt.message {
				t.Fatalf("first=%+v runs=%d calls=%d original=%+v", first, runs, calls, original)
			}
			if tt.followup && (len(results) != 1 || results[0].IsError != tt.resultError) {
				t.Fatalf("tool results: %+v", results)
			}
			if !tt.followup && (len(results) != 0 || first.ErrorMessage == "") {
				t.Fatalf("failure: %+v results=%+v", first, results)
			}
		})
	}
}
