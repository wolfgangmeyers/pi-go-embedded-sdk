package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// The marker is an in-process hint to a StreamFn consumer, not a wire error
// and never permission for this SDK to retry or refresh a bearer token.
func TestCodexPreOutput401FailureCause(t *testing.T) {
	const credential = "fake-bearer-cause-sentinel"
	request := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	for _, tc := range []struct {
		name      string
		transport bool
		partial   bool
		want      ai.FailureKind
	}{
		{"http 401 before start", false, false, ai.FailureCodexPreOutputUnauthorized},
		{"transport failure", true, false, ai.FailureUnknown},
		{"partial SSE unauthorized", false, true, ai.FailureUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, issues := 0, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
					t.Errorf("unexpected bearer header %q", got)
				}
				if tc.partial {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_token\",\"message\":\""+credential+"\"}}}\n\n")
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, credential)
			}))
			defer server.Close()
			opts := codexFixtureOptions()
			opts.MaxRetries = 4
			opts.HTTPClient = server.Client()
			opts.CodexAuth = func(context.Context) (ai.ModelAuth, error) {
				issues++
				return ai.ModelAuth{APIKey: credential, Headers: ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account")}}, nil
			}
			if tc.transport {
				opts.HTTPClient = &http.Client{Transport: codexFakeTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					return nil, errors.New("transport " + credential)
				})}
			}
			stream := ai.StreamSimple(context.Background(), codexFixtureModel(server.URL), request, opts)
			var events []ai.AssistantMessageEvent
			for ev := range stream.Events() {
				events = append(events, ev)
			}
			if issues != 1 || calls != 1 {
				t.Fatalf("bearer issues=%d HTTP attempts=%d, want one each", issues, calls)
			}
			if len(events) == 0 || events[len(events)-1].Type != ai.EventError || stream.Result().StopReason != ai.StopError {
				t.Fatalf("not terminal error: events=%v", events)
			}
			terminal := events[len(events)-1]
			if terminal.FailureKind != tc.want {
				t.Fatalf("failure kind=%v, want %v", terminal.FailureKind, tc.want)
			}
			if tc.partial {
				if len(events) < 3 || events[0].Type != ai.EventStart {
					t.Fatalf("expected partial stream before error: %v", events)
				}
			} else if len(events) != 1 {
				t.Fatalf("unexpected pre-start events: %v", events)
			}
			for _, value := range []any{terminal, events, stream.Result()} {
				b, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), credential) || strings.Contains(string(b), "failureKind") || strings.Contains(string(b), "codex_pre_output") {
					t.Fatalf("internal cause or credential leaked: %s", b)
				}
			}
			if strings.Contains(stream.Result().ErrorMessage, credential) {
				t.Fatal("credential leaked in error message")
			}
		})
	}
}
