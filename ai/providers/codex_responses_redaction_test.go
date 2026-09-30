package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

const codexFakeSecret = "fake-secret-sentinel"

type codexFakeTransport func(*http.Request) (*http.Response, error)

func (f codexFakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func assertCodexPublicRedacted(t *testing.T, stream *ai.AssistantMessageEventStream, preOutputUnauthorized bool) {
	t.Helper()
	var events []ai.AssistantMessageEvent
	for ev := range stream.Events() {
		events = append(events, ev)
	}
	result := stream.Result()
	if result == nil || result.StopReason != ai.StopError || len(events) == 0 || events[len(events)-1].Type != ai.EventError {
		t.Fatalf("unexpected terminal state: result=%#v events=%v", result, events)
	}
	for _, v := range []any{events, result, result.Diagnostics} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), codexFakeSecret) {
			t.Fatalf("fake marker leaked into SDK output: %s", b)
		}
	}
	if preOutputUnauthorized {
		if !strings.Contains(result.ErrorMessage, "401") || ai.IsRetryableAssistantError(*result) {
			t.Fatalf("pre-output 401 boundary lost: %q", result.ErrorMessage)
		}
	} else if strings.Contains(result.ErrorMessage, "401") && !strings.Contains(result.ErrorMessage, "outcome unknown") {
		t.Fatalf("unsafe unauthorized classification: %q", result.ErrorMessage)
	}
}

func TestCodexFakeOnlyErrorRedaction(t *testing.T) {
	request := ai.Context{Messages: []ai.Message{ai.NewUserText("hi", 1)}}
	t.Run("pre-output 401 body", func(t *testing.T) {
		calls := 0
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"`+codexFakeSecret+`"}}`)
		}))
		defer server.Close()
		opts := codexFixtureOptions()
		opts.MaxRetries = 3
		opts.HTTPClient = server.Client()
		assertCodexPublicRedacted(t, ai.StreamSimple(context.Background(), codexFixtureModel(server.URL), request, opts), true)
		if calls != 1 {
			t.Fatalf("replayed request: %d calls", calls)
		}
	})
	t.Run("pre-output transport error", func(t *testing.T) {
		opts := codexFixtureOptions()
		calls := 0
		opts.HTTPClient = &http.Client{Transport: codexFakeTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New(codexFakeSecret)
		})}
		assertCodexPublicRedacted(t, ai.StreamSimple(context.Background(), codexFixtureModel("https://example.invalid"), request, opts), false)
		if calls != 1 {
			t.Fatalf("replayed request: %d calls", calls)
		}
	})
	t.Run("partial SSE read error", func(t *testing.T) {
		calls := 0
		opts := codexFixtureOptions()
		opts.MaxRetries = 3
		opts.HTTPClient = &http.Client{Transport: codexFakeTransport(func(*http.Request) (*http.Response, error) {
			calls++
			body := io.MultiReader(strings.NewReader("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\n"), &codexErrReader{})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(body), Header: make(http.Header)}, nil
		})}
		stream := ai.StreamSimple(context.Background(), codexFixtureModel("https://example.invalid"), request, opts)
		var events []ai.AssistantMessageEvent
		for ev := range stream.Events() {
			events = append(events, ev)
		}
		result := stream.Result()
		if calls != 1 || result.StopReason != ai.StopError || len(events) < 3 || events[len(events)-1].Type != ai.EventError || !strings.Contains(result.ErrorMessage, "outcome unknown") || ai.IsRetryableAssistantError(*result) {
			t.Fatalf("partial stream improperly classified: calls=%d error=%q events=%v", calls, result.ErrorMessage, events)
		}
		for _, v := range []any{events, result, result.Diagnostics} {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), codexFakeSecret) {
				t.Fatalf("fake marker leaked into SDK output: %s", b)
			}
		}
	})
}

type codexErrReader struct{}

func (*codexErrReader) Read([]byte) (int, error) { return 0, errors.New(codexFakeSecret) }
