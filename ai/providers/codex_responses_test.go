package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

func codexFixtureModel(url string) *ai.Model {
	m := *ai.GetModel("openai-codex", "gpt-6-sol")
	m.BaseURL = url
	return &m
}

func codexFixtureOptions() *ai.SimpleStreamOptions {
	return &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{
		CodexAuth: func(context.Context) (ai.ModelAuth, error) {
			return ai.ModelAuth{APIKey: "fixture-token", Headers: ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account")}}, nil
		},
	}}}
}

func TestCodexOfflineSSEAndRequest(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/responses" || r.Method != "POST" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
			t.Errorf("auth headers missing")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "gpt-6-sol" || body["stream"] != true || body["store"] != false {
			t.Errorf("body missing Codex fields: %v", body)
		}
		if _, ok := body["input"].([]any); !ok {
			t.Errorf("input missing: %v", body)
		}
		if _, ok := body["tools"].([]any); !ok {
			t.Errorf("tools missing: %v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp_1"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"hello "}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"world"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"hello world"}]}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"/x\"}"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"/x\"}"}}`,
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer server.Close()
	model := codexFixtureModel(server.URL)
	req := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}, Tools: []ai.Tool{{Name: "read", Description: "read", Parameters: &ai.Schema{Type: "object"}}}}
	opts := codexFixtureOptions()
	opts.HTTPClient = server.Client()
	stream := ai.StreamSimple(context.Background(), model, req, opts)
	var events []ai.EventType
	for ev := range stream.Events() {
		events = append(events, ev.Type)
	}
	final := stream.Result()
	if final.StopReason != ai.StopToolUse || final.ResponseID != "resp_1" || len(final.Content) != 2 {
		t.Fatalf("final %#v; events %v", final, events)
	}
	text, ok := final.Content[0].(ai.TextContent)
	if !ok || text.Text != "hello world" {
		t.Fatalf("text %#v", final.Content[0])
	}
	tool, ok := final.Content[1].(ai.ToolCall)
	if !ok || tool.ID != "call_1|fc_1" || tool.Name != "read" {
		t.Fatalf("tool %#v", final.Content[1])
	}
	if fmt.Sprint(events) != fmt.Sprint([]ai.EventType{ai.EventStart, ai.EventTextStart, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextEnd, ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallDelta, ai.EventToolCallEnd, ai.EventDone}) {
		t.Fatalf("event order %v", events)
	}
}

func TestCodex401FailsClosedNoReplay(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "invalid token", http.StatusUnauthorized)
	}))
	defer server.Close()
	opts := codexFixtureOptions()
	opts.MaxRetries = 4
	opts.HTTPClient = server.Client()
	stream := ai.StreamSimple(context.Background(), codexFixtureModel(server.URL), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts)
	var events []ai.EventType
	for ev := range stream.Events() {
		events = append(events, ev.Type)
	}
	result := stream.Result()
	if calls != 1 || result.StopReason != ai.StopError || len(events) != 1 || events[0] != ai.EventError || !strings.Contains(result.ErrorMessage, "401") {
		t.Fatalf("calls=%d events=%v result=%#v", calls, events, result)
	}
}

// A 401 after output is not proof the turn was not accepted upstream. No
// adapter retry is safe here; router recovery must decide how to surface it.
func TestCodexPartialThenUnauthorizedDoesNotReplay(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"invalid_token\",\"message\":\"401 unauthorized\"}}}\n\n")
	}))
	defer server.Close()
	opts := codexFixtureOptions()
	opts.MaxRetries = 3
	opts.HTTPClient = server.Client()
	stream := ai.StreamSimple(context.Background(), codexFixtureModel(server.URL), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts)
	var events []ai.EventType
	for ev := range stream.Events() {
		events = append(events, ev.Type)
	}
	if calls != 1 || stream.Result().StopReason != ai.StopError || !strings.Contains(stream.Result().ErrorMessage, "401 unauthorized after stream start; turn outcome unknown") || events[len(events)-1] != ai.EventError {
		t.Fatalf("calls=%d events=%v result=%#v", calls, events, stream.Result())
	}
}

func TestCodexDoesNotUseAPIKeyFallback(t *testing.T) {
	model := codexFixtureModel("https://fixture.invalid")
	opts := &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{ProviderRequestOptions: ai.ProviderRequestOptions{APIKey: "must-not-be-used"}}}
	final := ai.StreamSimple(context.Background(), model, ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts).Result()
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "token source") {
		t.Fatalf("unexpected auth fallback: %#v", final)
	}
}

func TestCodexCancellationClosesStream(t *testing.T) {
	entered := make(chan struct{})
	closed := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\"}}\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(closed)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	opts := codexFixtureOptions()
	opts.HTTPClient = server.Client()
	stream := ai.StreamSimple(ctx, codexFixtureModel(server.URL), ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}, opts)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	var events []ai.EventType
	for ev := range stream.Events() {
		events = append(events, ev.Type)
	}
	if final := stream.Result(); final.StopReason != ai.StopAborted {
		t.Fatalf("result %#v; events %v", final, events)
	}
	for _, ev := range events {
		if ev == ai.EventDone {
			t.Fatalf("fabricated completion: %v", events)
		}
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP body not closed on cancel")
	}
}
