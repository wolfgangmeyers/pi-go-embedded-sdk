package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
)

// A failed automatic summary on a non-ordered session keeps the original
// context and still sends the user's request to the model.
func TestAutomaticCompactionSummaryFailureContinuesNonOrdered(t *testing.T) {
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{
		Models: []providers.FauxModelDefinition{{ID: "faux-compaction-failure", ContextWindow: 1000}},
	})
	defer reg.Unregister()

	var continuation ai.TranscriptContext
	requests := 0
	reg.SetResponses([]providers.FauxResponseStep{
		func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			requests++
			if system, ok := ai.GetInitialSystemMessage(req.Messages); !ok || ai.GetSystemMessageText(system) != summarizationSystemPrompt {
				t.Errorf("first request was not summarization")
			}
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "failed"}}, ai.StopError)
		},
		func(req ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			requests++
			continuation = req
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "continued"}}, ai.StopStop)
		},
	})

	s := NewSession(SessionOptions{Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: NoToolsAll})
	s.EnableCompaction(CompactionSettings{Enabled: true, ReserveTokens: 200, KeepRecentTokens: 1500})
	big := strings.Repeat("history", 600)
	var history []agent.AgentMessage
	for i := 0; i < 6; i++ {
		history = append(history,
			ai.NewUserText(big, int64(2*i+1)),
			ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: big}}, StopReason: ai.StopStop, Timestamp: int64(2*i + 2)},
		)
	}
	s.Agent.SetMessages(history)
	result, err := s.Run(context.Background(), "next question")
	if err != nil || result.Text != "continued" {
		t.Fatalf("automatic summary failure aborted continuation: result=%+v err=%v", result, err)
	}
	if requests != 2 || reg.PendingResponseCount() != 0 {
		t.Fatalf("requests=%d pending=%d, want failed summary then continuation", requests, reg.PendingResponseCount())
	}
	if s.compactState.checkpoint != nil {
		t.Fatal("failed summary published a checkpoint")
	}
	conversation := withoutSystemMessages(continuation.Messages)
	if len(conversation) != len(history)+1 {
		t.Fatalf("continuation lost history: got %d conversation messages, want %d", len(conversation), len(history)+1)
	}
	for i, want := range history {
		if conversation[i].MessageRole() != want.MessageRole() {
			t.Fatalf("history[%d] role changed: got %s want %s", i, conversation[i].MessageRole(), want.MessageRole())
		}
		if i%2 == 0 && userText(conversation[i]) != big {
			t.Fatalf("history[%d] text changed", i)
		}
	}
	if userText(conversation[len(conversation)-1]) != "next question" {
		t.Fatal("continuation lost current user prompt")
	}
}
