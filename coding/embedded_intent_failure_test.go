package coding_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/coding"
)

// The host rejects durable intent admission. This is not a permission denial:
// the tool call must terminate the turn, not become a tool result for the model.
func TestEmbeddedIntentAdmissionFailureAbortsWithoutExecuteOrReplay(t *testing.T) {
	ctx := context.Background()
	h := openConversation(t, filepath.Join(t.TempDir(), "intent.sqlite"), "intent-epoch")
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	providerCalls := 0
	reg.SetResponses([]providers.FauxResponseStep{
		func(_ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			providerCalls++
			return providers.FauxAssistantMessage(ai.ContentList{providers.FauxToolCall("external", map[string]any{}, "effect-1")}, ai.StopToolUse)
		},
		func(_ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			providerCalls++
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "incorrect continuation"}}, ai.StopStop)
		},
	})
	executes, admits := 0, 0
	tool := agent.AgentTool{Name: "external", Description: "external side effect", Parameters: &ai.Schema{Type: "object"}, Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		executes++
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "effect"}}}, nil
	}}
	opts := coding.SessionOptions{SessionID: h.session, Model: reg.GetModel(), Cwd: t.TempDir(), Tools: []agent.AgentTool{tool}, OrderedPersistence: h,
		BeforeToolExecute: func(_ context.Context, c agent.BeforeToolCallContext) error {
			admits++
			if c.ToolCall.ID != "effect-1" || c.ToolCall.Name != "external" {
				t.Errorf("unexpected intent: %+v", c.ToolCall)
			}
			if got := strings.Join(roles(t, h), ","); got != "system,user,assistant" {
				t.Errorf("intent before assistant commit: %s", got)
			}
			return errors.New("durable intent admission failed")
		},
	}
	s := coding.NewSession(opts)
	var events []agent.EventType
	s.Subscribe(func(_ context.Context, e agent.AgentEvent) error { events = append(events, e.Type); return nil })
	result, err := s.Run(ctx, "perform effect")
	if err == nil || !strings.Contains(err.Error(), "durable intent admission failed") {
		t.Fatalf("missing failure: result=%+v err=%v", result, err)
	}
	if result == nil || result.ErrorMessage == "" || result.StopReason != ai.StopError {
		t.Fatalf("not a failed turn: %+v", result)
	}
	if executes != 0 || admits != 1 || providerCalls != 1 {
		t.Fatalf("execute=%d admission=%d provider=%d", executes, admits, providerCalls)
	}
	for _, m := range s.History() {
		if m.MessageRole() == ai.RoleToolResult {
			t.Fatalf("intent failure became tool result: %+v", m)
		}
	}
	var turnEnd, agentEnd bool
	for _, e := range events {
		if e == agent.EvTurnEnd {
			turnEnd = true
		}
		if e == agent.EvAgentEnd {
			agentEnd = true
		}
	}
	if !turnEnd || !agentEnd {
		t.Fatalf("missing terminal events: %v", events)
	}
	snap, err := h.snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(roles(t, h), ","); got != "system,user,assistant" {
		t.Fatalf("uncommitted or replayable result: %s", got)
	}
	reopened := coding.NewSession(opts)
	if err := reopened.RestoreOrdered(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if executes != 0 || admits != 1 || providerCalls != 1 {
		t.Fatalf("restore replayed effect: execute=%d admission=%d provider=%d", executes, admits, providerCalls)
	}
	if err := reopened.Continue(ctx); err == nil {
		t.Fatal("restored assistant tool call auto-continued")
	}
	if executes != 0 || providerCalls != 1 {
		t.Fatalf("continue replayed effect: execute=%d provider=%d", executes, providerCalls)
	}
}
