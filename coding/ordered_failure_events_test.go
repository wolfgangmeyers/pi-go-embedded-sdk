package coding_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/coding"
)

func TestOrderedFailureLifecycleDoesNotRecordUncommittedFailure(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "conversation.sqlite"), "events")
	h.failAppend = true
	s, done := fixture(t, h, answer("unused"))
	defer done()
	rec, err := coding.StartSession(t.TempDir(), s.Model)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	s.Record(rec)
	var ends []agent.AgentMessage
	var turn, terminal bool
	s.Agent.Subscribe(func(_ context.Context, e agent.AgentEvent) error {
		switch e.Type {
		case agent.EvMessageEnd:
			ends = append(ends, e.Message)
		case agent.EvTurnEnd:
			if m, ok := e.Message.(*ai.AssistantMessage); ok && strings.Contains(m.ErrorMessage, "injected append") {
				turn = true
			}
		case agent.EvAgentEnd:
			terminal = true
		}
		return nil
	})
	result, err := s.Run(context.Background(), "uncommitted")
	if err == nil || result == nil || result.StopReason != ai.StopError {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(ends) != 0 || !turn || !terminal {
		t.Fatalf("message ends=%v turn=%v agent=%v", ends, turn, terminal)
	}
	data, err := os.ReadFile(rec.Path())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "uncommitted") || strings.Contains(string(data), "injected append") {
		t.Fatalf("recorded uncommitted message: %s", data)
	}
}

func TestRunFailureTextDoesNotReuseEarlierAssistant(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "conversation.sqlite"), "stale")
	s, done := fixture(t, h, answer("old answer"))
	defer done()
	if result, err := s.Run(context.Background(), "first"); err != nil || result.Text != "old answer" {
		t.Fatalf("first: %+v %v", result, err)
	}
	h.failAppend = true
	result, err := s.Run(context.Background(), "second")
	if err == nil || result == nil || result.Text != "" || result.StopReason != ai.StopError {
		t.Fatalf("failed: %+v %v", result, err)
	}
}

func TestRunFailureTextDoesNotReuseIntermediateAssistant(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "conversation.sqlite"), "intermediate")
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	reg.SetResponses([]providers.FauxResponseStep{providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "intermediate"}, providers.FauxToolCall("test", map[string]any{}, "call")}, ai.StopToolUse)), func(_ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		return &ai.AssistantMessage{StopReason: ai.StopError, ErrorMessage: "provider failed"}
	}})
	tool := agent.AgentTool{Name: "test", Parameters: &ai.Schema{Type: "object"}, Execute: func(context.Context, string, map[string]any, agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{}, nil
	}}
	s := coding.NewSession(coding.SessionOptions{SessionID: h.session, Model: reg.GetModel(), Cwd: t.TempDir(), Tools: []agent.AgentTool{tool}, OrderedPersistence: h})
	result, err := s.Run(context.Background(), "go")
	if err == nil || result == nil || result.Text != "" || result.StopReason != ai.StopError {
		t.Fatalf("failed: %+v %v", result, err)
	}
}
