package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestCancellationAfterAdmissionResolvesReplay(t *testing.T) {
	for _, mode := range []struct {
		name      string
		execution ToolExecutionMode
	}{{"sequential", ToolSequential}, {"parallel", ToolParallel}} {
		for _, scenario := range []string{"result", "error", "cancelled", "cancel-during-replay", "panic"} {
			t.Run(mode.name+"/"+scenario, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				executed, replayed, ends, results := false, false, 0, 0
				tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
					executed = true
					return AgentToolResult{}, nil
				}}
				call := ai.ToolCall{ID: "one", Name: "effect", Arguments: map[string]any{}}
				msg := &ai.AssistantMessage{Content: ai.ContentList{call}}
				saved := &ai.ToolResultMessage{ToolCallID: "one", ToolName: "effect", Content: ai.ContentList{ai.TextContent{Text: "saved"}}, Timestamp: 123}
				cfg := AgentLoopConfig{ToolExecution: mode.execution, BeforeToolExecute: func(context.Context, BeforeToolCallContext) error {
					if scenario != "cancel-during-replay" {
						cancel()
					}
					return nil
				}, ReplayToolResult: func(context.Context, BeforeToolCallContext) (*ai.ToolResultMessage, error) {
					replayed = true
					switch scenario {
					case "result":
						return saved, nil
					case "error":
						return nil, errors.New("replay failed")
					case "cancel-during-replay":
						cancel()
					case "panic":
						panic("replay panic")
					}
					return nil, nil
				}}
				var batch executedBatch
				var failure any
				func() {
					defer func() { failure = recover() }()
					batch = executeToolCalls(ctx, &AgentContext{Tools: []AgentTool{tool}}, msg, cfg, func(e AgentEvent) error {
						if e.Type == EvToolExecutionEnd {
							ends++
						}
						if e.Type == EvMessageEnd {
							results++
						}
						return nil
					})
				}()
				if !replayed || executed {
					t.Fatalf("replayed=%v executed=%v", replayed, executed)
				}
				if scenario == "result" {
					if failure != nil || len(batch.messages) != 1 || batch.messages[0].Timestamp != 123 || ends != 1 {
						t.Fatalf("failure=%v batch=%+v ends=%d", failure, batch, ends)
					}
					return
				}
				abort, ok := failure.(toolAdmissionAbort)
				if !ok {
					t.Fatalf("failure=%#v", failure)
				}
				if scenario == "cancelled" || scenario == "cancel-during-replay" {
					if !errors.Is(abort.err, context.Canceled) {
						t.Fatalf("error=%v", abort.err)
					}
				}
				if ends != 0 || results != 0 {
					t.Fatalf("published end=%d message=%d", ends, results)
				}
			})
		}
	}
}

func TestCancellationAfterAdmissionWithoutReplayIsFatal(t *testing.T) {
	for _, mode := range []ToolExecutionMode{ToolSequential, ToolParallel} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			executed := false
			tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
				executed = true
				return AgentToolResult{}, nil
			}}
			call := ai.ToolCall{ID: "one", Name: "effect", Arguments: map[string]any{}}
			var failure any
			func() {
				defer func() { failure = recover() }()
				executeToolCalls(ctx, &AgentContext{Tools: []AgentTool{tool}}, &ai.AssistantMessage{Content: ai.ContentList{call}}, AgentLoopConfig{ToolExecution: mode, BeforeToolExecute: func(context.Context, BeforeToolCallContext) error { cancel(); return nil }}, func(AgentEvent) error { return nil })
			}()
			abort, ok := failure.(toolAdmissionAbort)
			if !ok || !errors.Is(abort.err, context.Canceled) || executed {
				t.Fatalf("failure=%v executed=%v", failure, executed)
			}
		})
	}
}

func TestAdmittedBodyRechecksCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	executed := false
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
		executed = true
		return AgentToolResult{}, nil
	}}
	prep := prepareToolCall(ctx, &AgentContext{Tools: []AgentTool{tool}}, nil, ai.ToolCall{ID: "one", Name: "effect", Arguments: map[string]any{}}, ToolCallHooks{BeforeToolExecute: func(context.Context, BeforeToolCallContext) error { return nil }}, []AgentTool{tool})
	cancel()
	defer func() {
		abort, ok := recover().(toolAdmissionAbort)
		if !ok || !errors.Is(abort.err, context.Canceled) || executed {
			t.Fatalf("abort=%v executed=%v", abort.err, executed)
		}
	}()
	executePreparedToolCall(ctx, *prep.prepared, func(AgentToolResult) error { return nil })
}

// Cancel between the initial cancellation check and replay in the replay-only path.
type cancelOnSecondErr struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelOnSecondErr) Err() error {
	c.checks++
	if c.checks == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestReplayOnlyDoesNotStartReplayAfterCancellation(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnSecondErr{Context: base, cancel: cancel}
	replayed := false
	tool := AgentTool{Name: "effect", Parameters: &ai.Schema{Type: "object"}}
	prep := prepareToolCall(ctx, &AgentContext{Tools: []AgentTool{tool}}, nil, ai.ToolCall{ID: "one", Name: "effect", Arguments: map[string]any{}}, ToolCallHooks{ReplayToolResult: func(context.Context, BeforeToolCallContext) (*ai.ToolResultMessage, error) {
		replayed = true
		return nil, nil
	}}, []AgentTool{tool})
	if replayed || prep.prepared == nil {
		t.Fatalf("replayed=%v prep=%+v", replayed, prep)
	}
}

func TestCancelledOrderedPersistenceRejectsPublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := NewAgent(AgentOptions{})
	var persisted []AgentMessage
	a.BeforeMessageEnd = func(ctx context.Context, m AgentMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		persisted = append(persisted, m)
		return nil
	}
	first, second := &ai.UserMessage{}, &ai.UserMessage{}
	sink := a.processEvent(ctx)
	if err := sink(AgentEvent{Type: EvMessageEnd, Message: first}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := sink(AgentEvent{Type: EvMessageEnd, Message: second}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if len(persisted) != 1 || persisted[0] != first || len(a.State().Messages) != 1 || a.State().Messages[0] != first {
		t.Fatalf("persisted=%v state=%v", persisted, a.State().Messages)
	}
}
