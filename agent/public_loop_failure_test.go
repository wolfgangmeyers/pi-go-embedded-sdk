package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

func TestPublicLoopAdmissionLifecycle(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		for _, mode := range []string{"cancel", "fatal", "success"} {
			t.Run(fmt.Sprintf("continue=%v/%s", continuation, mode), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ran := false
				tool := agent.AgentTool{Name: "echo", Parameters: ai.Object(), Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
					ran = true
					return agent.AgentToolResult{Terminate: true}, nil
				}}
				cfg := agent.AgentLoopConfig{Model: &ai.Model{ID: "mock", Provider: "mock"}, BeforeToolExecute: func(context.Context, agent.BeforeToolCallContext) error {
					switch mode {
					case "cancel":
						cancel()
						return ctx.Err()
					case "fatal":
						return errors.New("admission failed")
					}
					return nil
				}}
				streamFn := func(context.Context, *ai.Model, ai.TranscriptContext, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
					s := ai.NewAssistantMessageEventStream()
					s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Message: &ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "1", Name: "echo", Arguments: map[string]any{}}}, StopReason: ai.StopToolUse}})
					return s
				}
				prompt := ai.NewUserText("hi", 1)
				ac := agent.AgentContext{Tools: []agent.AgentTool{tool}}
				var s *ai.EventStream[agent.AgentEvent, []agent.AgentMessage]
				if continuation {
					ac.Messages = []agent.AgentMessage{prompt}
					var err error
					s, err = agent.AgentLoopContinue(ctx, ac, cfg, streamFn)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					s = agent.AgentLoop(ctx, []agent.AgentMessage{prompt}, ac, cfg, streamFn)
				}
				events := make(chan []agent.AgentEvent, 1)
				result := make(chan []agent.AgentMessage, 1)
				go func() {
					var all []agent.AgentEvent
					for e := range s.Events() {
						all = append(all, e)
					}
					events <- all
				}()
				go func() { result <- s.Result() }()
				var es []agent.AgentEvent
				select {
				case es = <-events:
				case <-time.After(3 * time.Second):
					t.Fatal("events blocked")
				}
				var messages []agent.AgentMessage
				select {
				case messages = <-result:
				case <-time.After(3 * time.Second):
					t.Fatal("result blocked")
				}
				if len(es) == 0 || es[len(es)-1].Type != agent.EvAgentEnd || len(messages) != len(es[len(es)-1].Messages) {
					t.Fatalf("bad terminal events/result: %v %v", es, messages)
				}
				if ran != (mode == "success") {
					t.Fatalf("ran=%v", ran)
				}
				if mode != "success" {
					if len(es) < 4 || es[len(es)-2].Type != agent.EvTurnEnd || es[len(es)-3].Type != agent.EvMessageEnd || es[len(es)-4].Type != agent.EvMessageStart {
						t.Fatalf("bad failure lifecycle: %v", es)
					}
					failure, ok := es[len(es)-2].Message.(*ai.AssistantMessage)
					if !ok {
						t.Fatal("missing failure")
					}
					want := ai.StopError
					text := "admission failed"
					if mode == "cancel" {
						want = ai.StopAborted
						text = "context canceled"
					}
					if failure.StopReason != want || !strings.Contains(failure.ErrorMessage, text) || messages[len(messages)-1] != failure {
						t.Fatalf("bad failure: %+v", failure)
					}
				}
			})
		}
	}
}

func TestPublicLoopParallelFatalJoinsSibling(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		t.Run(fmt.Sprint(continuation), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			failed := make(chan struct{})
			tool := agent.AgentTool{Name: "echo", Parameters: ai.Object(), Execute: func(_ context.Context, id string, _ map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
				if id == "sibling" {
					close(started)
					<-release
				}
				return agent.AgentToolResult{}, nil
			}}
			cfg := agent.AgentLoopConfig{Model: &ai.Model{ID: "mock"}, ToolExecution: agent.ToolParallel, BeforeToolExecute: func(_ context.Context, c agent.BeforeToolCallContext) error {
				if c.ToolCall.ID == "fatal" {
					<-started
					close(failed)
					return errors.New("admission failed")
				}
				return nil
			}}
			fn := func(context.Context, *ai.Model, ai.TranscriptContext, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
				s := ai.NewAssistantMessageEventStream()
				s.Push(ai.AssistantMessageEvent{Type: ai.EventDone, Message: &ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "sibling", Name: "echo", Arguments: map[string]any{}}, ai.ToolCall{ID: "fatal", Name: "echo", Arguments: map[string]any{}}}, StopReason: ai.StopToolUse}})
				return s
			}
			p := ai.NewUserText("hi", 1)
			ac := agent.AgentContext{Tools: []agent.AgentTool{tool}}
			var s *ai.EventStream[agent.AgentEvent, []agent.AgentMessage]
			if continuation {
				ac.Messages = []agent.AgentMessage{p}
				s, _ = agent.AgentLoopContinue(context.Background(), ac, cfg, fn)
			} else {
				s = agent.AgentLoop(context.Background(), []agent.AgentMessage{p}, ac, cfg, fn)
			}
			done := make(chan []agent.AgentEvent, 1)
			go func() {
				var events []agent.AgentEvent
				for e := range s.Events() {
					events = append(events, e)
				}
				done <- events
			}()
			select {
			case <-failed:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("fatal hook not reached")
			}
			select {
			case <-done:
				close(release)
				t.Fatal("ended before sibling joined")
			default:
			}
			close(release)
			select {
			case events := <-done:
				if events[len(events)-1].Type != agent.EvAgentEnd {
					t.Fatal("missing terminal event")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stream blocked")
			}
		})
	}
}

func TestPublicLoopPrematureProviderClose(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		for _, mode := range []string{"no-start", "partial", "malformed-done", "malformed-error", "cancel"} {
			t.Run(fmt.Sprintf("continue=%v/%s", continuation, mode), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				fn := func(context.Context, *ai.Model, ai.TranscriptContext, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
					s := ai.NewAssistantMessageEventStream()
					switch mode {
					case "partial", "cancel":
						s.Push(ai.AssistantMessageEvent{Type: ai.EventStart, Partial: &ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "unfinished"}}}})
						if mode == "cancel" {
							cancel()
						}
						s.End()
					case "malformed-done":
						s.Push(ai.AssistantMessageEvent{Type: ai.EventDone})
					case "malformed-error":
						s.Push(ai.AssistantMessageEvent{Type: ai.EventError})
					default:
						s.End()
					}
					return s
				}
				prompt := ai.NewUserText("hi", 1)
				ac := agent.AgentContext{}
				cfg := agent.AgentLoopConfig{Model: &ai.Model{ID: "mock", Provider: "mock"}}
				var s *ai.EventStream[agent.AgentEvent, []agent.AgentMessage]
				if continuation {
					ac.Messages = []agent.AgentMessage{prompt}
					var err error
					s, err = agent.AgentLoopContinue(ctx, ac, cfg, fn)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					s = agent.AgentLoop(ctx, []agent.AgentMessage{prompt}, ac, cfg, fn)
				}
				done := make(chan []agent.AgentEvent, 1)
				go func() {
					var events []agent.AgentEvent
					for e := range s.Events() {
						events = append(events, e)
					}
					done <- events
				}()
				var events []agent.AgentEvent
				select {
				case events = <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("stream blocked")
				}
				messages := s.Result()
				expected := 1
				if !continuation {
					expected++
				}
				if len(messages) != expected {
					t.Fatalf("unexpected transcript: %#v", messages)
				}
				failure, ok := messages[len(messages)-1].(*ai.AssistantMessage)
				if !ok || failure == nil {
					t.Fatalf("nil or invalid failure: %#v", messages)
				}
				want, text := ai.StopError, "provider stream"
				if mode == "cancel" {
					want, text = ai.StopAborted, "context canceled"
				}
				if failure.StopReason != want || !strings.Contains(failure.ErrorMessage, text) {
					t.Fatalf("bad failure: %+v", failure)
				}
				if len(events) < 2 || events[len(events)-1].Type != agent.EvAgentEnd || events[len(events)-2].Type != agent.EvTurnEnd {
					t.Fatalf("bad lifecycle: %#v", events)
				}
				for _, e := range events {
					if e.Type == agent.EvMessageEnd {
						if m, ok := e.Message.(*ai.AssistantMessage); ok && m == nil {
							t.Fatal("typed nil message")
						}
					}
				}
			})
		}
	}
}
