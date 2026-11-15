package coding_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/coding"
)

func TestEmbeddedPolicyDenialCheckedResultAndAppendFailure(t *testing.T) {
	for _, failAppend := range []bool{false, true} {
		name := "committed"
		if failAppend {
			name = "append-failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			h := openConversation(t, filepath.Join(t.TempDir(), "policy.sqlite"), "policy-epoch")
			reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
			defer reg.Unregister()
			calls, executes := 0, 0
			reg.SetResponses([]providers.FauxResponseStep{
				providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{providers.FauxToolCall("external", map[string]any{}, "original-call")}, ai.StopToolUse)),
				func(history ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
					calls++
					if got := strings.Join(roles(t, h), ","); got != "system,user,assistant,toolResult" {
						t.Errorf("unchecked result: %s", got)
					}
					var found bool
					for _, message := range history.Messages {
						if result, ok := message.(ai.ToolResultMessage); ok && result.ToolCallID == "original-call" {
							found = true
							if result.ToolName != "external" || !result.IsError {
								t.Errorf("uncorrelated denial: %+v", result)
							}
						}
					}
					if !found {
						t.Error("missing denied result")
					}
					return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "done"}}, ai.StopStop)
				},
			})
			tool := agent.AgentTool{Name: "external", Parameters: &ai.Schema{Type: "object"}, Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
				executes++
				return agent.AgentToolResult{}, nil
			}}
			s := coding.NewSession(coding.SessionOptions{SessionID: h.session, Model: reg.GetModel(), Cwd: t.TempDir(), Tools: []agent.AgentTool{tool}, OrderedPersistence: h,
				BeforeToolExecute: func(_ context.Context, c agent.BeforeToolCallContext) error {
					if c.ToolCall.ID != "original-call" {
						t.Errorf("wrong call: %s", c.ToolCall.ID)
					}
					if got := strings.Join(roles(t, h), ","); got != "system,user,assistant" {
						t.Errorf("admission before assistant commit: %s", got)
					}
					h.failAppend = failAppend
					return &agent.PolicyDenial{Reason: "strict path policy"}
				},
			})
			_, err := s.Run(ctx, "attempt")
			if failAppend {
				if err == nil || !strings.Contains(err.Error(), "injected append transaction failure") {
					t.Fatalf("append failure swallowed: %v", err)
				}
				h.failAppend = false
				if got := strings.Join(roles(t, h), ","); got != "system,user,assistant" {
					t.Fatalf("fabricated result: %s", got)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := h.snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Messages) != 5 {
					t.Fatalf("missing durable continuation: %d", len(snapshot.Messages))
				}
				reopened := coding.NewSession(coding.SessionOptions{SessionID: h.session, Model: reg.GetModel(), Cwd: t.TempDir(), Tools: []agent.AgentTool{tool}, OrderedPersistence: h})
				if err := reopened.RestoreOrdered(ctx, snapshot); err != nil {
					t.Fatal(err)
				}
				result, ok := reopened.History()[3].(ai.ToolResultMessage)
				if !ok || !result.IsError || result.ToolCallID != "original-call" || result.ToolName != "external" {
					t.Fatalf("lost denial: %+v", reopened.History()[3])
				}
			}
			if executes != 0 {
				t.Fatalf("executed denied effect %d", executes)
			}
			expected := 1
			if failAppend {
				expected = 0
			}
			if calls != expected {
				t.Fatalf("provider continuations %d, want %d", calls, expected)
			}
		})
	}
}
