package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// The automatic compaction summary must use the session's Codex token source,
// just like the following normal turn. Neither request can fall back to APIKey.
func TestEmbeddedCodexAutomaticCompactionRequestAuth(t *testing.T) {
	const token = "synthetic-compaction-codex-token"
	const summary = "synthetic checkpoint from Codex"
	var authCalls atomic.Int32
	var mu sync.Mutex
	var requestKinds []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/codex/responses" ||
			r.Header.Get("Authorization") != "Bearer "+token ||
			r.Header.Get("ChatGPT-Account-ID") != "synthetic-account" {
			t.Error("unexpected Codex request method, path or auth headers")
		}
		var payload struct {
			Instructions string          `json:"instructions"`
			Input        json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode Codex request: %v", err)
			return
		}
		kind, text := "normal", "offline continuation"
		if payload.Instructions == summarizationSystemPrompt {
			kind, text = "summary", summary
		} else if !strings.Contains(string(payload.Input), summary) {
			t.Error("normal request did not receive compacted checkpoint")
		}
		mu.Lock()
		requestKinds = append(requestKinds, kind)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\"}}\n\n")
		encoded, _ := json.Marshal(text)
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":%s}\n\n", encoded)
		fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\",\"content\":[{\"type\":\"output_text\",\"text\":%s}]}}\n\n", encoded)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_mock\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	model := *ai.GetModel("openai-codex", "gpt-6-sol")
	model.BaseURL = server.URL
	model.ContextWindow = 1000
	s := NewSession(SessionOptions{
		Model: &model, Cwd: t.TempDir(), NoTools: NoToolsAll,
		APIKey: "not-a-codex-fallback",
		CodexAuth: func(context.Context) (ai.ModelAuth, error) {
			authCalls.Add(1)
			return ai.ModelAuth{APIKey: token, Headers: ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("synthetic-account")}}, nil
		},
	})
	// Match the existing Codex session fixture: only the local TLS fake is trusted.
	s.Agent.HTTPClient = server.Client()
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
	if err != nil || result == nil || result.Text != "offline continuation" {
		t.Fatalf("Codex compaction turn failed: err=%v result=%+v", err, result)
	}
	mu.Lock()
	kinds := append([]string(nil), requestKinds...)
	mu.Unlock()
	if len(kinds) != 2 || kinds[0] != "summary" || kinds[1] != "normal" || authCalls.Load() != 2 {
		t.Fatalf("want authenticated summary then normal request: kinds=%v auth calls=%d", kinds, authCalls.Load())
	}
	if s.compactState.checkpoint == nil || !strings.Contains(s.compactState.checkpoint.summary, summary) {
		t.Fatal("automatic compaction did not publish the Codex summary")
	}
	if strings.Contains(fmt.Sprint(s.History()), token) {
		t.Fatal("auth token leaked into session history")
	}
}
