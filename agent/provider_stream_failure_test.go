package agent_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

func TestProviderStreamFailuresAcrossEntrypoints(t *testing.T) {
	for _, entry := range []string{"loop", "continue", "agent"} {
		for _, mode := range []string{"error-tool-use", "error-aborted", "close", "missing-terminal"} {
			t.Run(fmt.Sprintf("%s/%s", entry, mode), func(t *testing.T) {
				var runs atomic.Int32
				tool := agent.AgentTool{Name: "danger", Parameters: ai.Object(), Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
					runs.Add(1)
					return agent.AgentToolResult{Terminate: true}, nil
				}}
				first := &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "old"}}, ResponseID: "old"}
				latest := &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "latest"}, ai.ToolCall{ID: "1", Name: "danger", Arguments: map[string]any{}}}, ResponseID: "response-1", Model: "actual", Usage: ai.Usage{Input: 42}, StopReason: ai.StopToolUse}
				fn := func(context.Context, *ai.Model, ai.TranscriptContext, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
					s := ai.NewAssistantMessageEventStream()
					s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: first})
					s.Push(ai.AssistantMessageEvent{Type: ai.EventTextDelta, Partial: latest})
					switch mode {
					case "error-tool-use":
						s.Push(ai.AssistantMessageEvent{Type: ai.EventError, Error: latest})
					case "error-aborted":
						m := latest.Clone()
						m.StopReason = ai.StopAborted
						s.Push(ai.AssistantMessageEvent{Type: ai.EventError, Error: m})
					case "missing-terminal":
						s.Push(ai.AssistantMessageEvent{Type: ai.EventDone})
					default:
						s.End()
					}
					return s
				}
				model := &ai.Model{ID: "mock", Provider: "mock"}
				var messages []agent.AgentMessage
				if entry == "agent" {
					a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: model, Tools: []agent.AgentTool{tool}}, StreamFn: fn})
					if err := a.Prompt(context.Background(), "hello"); err != nil {
						t.Fatal(err)
					}
					messages = a.State().Messages
				} else {
					ac := agent.AgentContext{Tools: []agent.AgentTool{tool}}
					cfg := agent.AgentLoopConfig{Model: model}
					prompt := ai.NewUserText("hello", 1)
					var stream *ai.EventStream[agent.AgentEvent, []agent.AgentMessage]
					if entry == "continue" {
						ac.Messages = []agent.AgentMessage{prompt}
						var err error
						stream, err = agent.AgentLoopContinue(context.Background(), ac, cfg, fn)
						if err != nil {
							t.Fatal(err)
						}
					} else {
						stream = agent.AgentLoop(context.Background(), []agent.AgentMessage{prompt}, ac, cfg, fn)
					}
					for range stream.Events() {
					}
					messages = stream.Result()
				}
				if runs.Load() != 0 {
					t.Fatal("tool executed on failed stream")
				}
				m, ok := messages[len(messages)-1].(*ai.AssistantMessage)
				if !ok || m == nil {
					t.Fatalf("missing assistant: %#v", messages)
				}
				want := ai.StopError
				if mode == "error-aborted" {
					want = ai.StopAborted
				}
				if m.StopReason != want {
					t.Fatalf("stop: %+v", m)
				}
				if mode == "close" || mode == "missing-terminal" {
					if m.ResponseID != "response-1" || m.Model != "actual" || m.Usage.Input != 42 || len(m.Content) != 1 || m.Content[0].(ai.TextContent).Text != "latest" || !strings.Contains(m.ErrorMessage, "provider stream") {
						t.Fatalf("lost partial: %+v", m)
					}
				}
			})
		}
	}
}
