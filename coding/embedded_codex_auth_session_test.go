package coding

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestEmbeddedCodexSessionRequestAuth(t *testing.T) {
	const token = "synthetic-codex-token"
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/codex/responses" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ChatGPT-Account-ID") != "synthetic-account" {
			t.Errorf("unexpected Codex request method, path or auth headers")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"offline ok\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\",\"content\":[{\"type\":\"output_text\",\"text\":\"offline ok\"}]}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_mock\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	model := *ai.GetModel("openai-codex", "gpt-6-sol")
	model.BaseURL = server.URL
	newSession := func(auth func(context.Context) (ai.ModelAuth, error)) *Session {
		s := NewSession(SessionOptions{
			Model: &model, Cwd: t.TempDir(), NoTools: NoToolsAll,
			APIKey: "not-a-codex-fallback", CodexAuth: auth,
		})
		// Only the fake TLS server is reachable; no real provider or credentials.
		s.Agent.HTTPClient = server.Client()
		return s
	}

	calls := 0
	s := newSession(func(context.Context) (ai.ModelAuth, error) {
		calls++
		return ai.ModelAuth{APIKey: token, Headers: ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("synthetic-account")}}, nil
	})
	result, err := s.Run(context.Background(), "offline request")
	if err != nil || result == nil || result.Text != "offline ok" || requests.Load() != 1 || calls != 1 {
		t.Fatalf("session Codex request failed: err=%v result=%+v requests=%d auth calls=%d", err, result, requests.Load(), calls)
	}
	if strings.Contains(fmt.Sprint(s.History()), token) {
		t.Fatal("auth token leaked into session history")
	}

	missing := newSession(nil)
	_, err = missing.Run(context.Background(), "should not reach HTTP")
	if err == nil || !strings.Contains(err.Error(), "token source is required") || requests.Load() != 1 {
		t.Fatalf("missing Codex auth did not fail before HTTP: err=%v requests=%d", err, requests.Load())
	}
}
