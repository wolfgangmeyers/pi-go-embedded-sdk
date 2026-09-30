package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

// Offline request-shape contract against the installed Pi Codex adapter, not
// evidence of the remote service's HTTP 400 validation rules.
func TestCodexLeadingInstructionsOffline(t *testing.T) {
	base := []ai.Message{ai.NewSystemText("root instructions", 1), ai.NewUserText("first user", 2)}
	continued := append([]ai.Message{}, base...)
	continued = append(continued,
		ai.AssistantMessage{Api: ai.APIOpenAICodexResponses, Provider: "openai-codex", Model: "gpt-6-sol", Content: ai.ContentList{ai.ToolCall{ID: "call_1|fc_1", Name: "read", Arguments: map[string]any{"path": "/x"}}}, StopReason: ai.StopToolUse, Timestamp: 3},
		ai.ToolResultMessage{ToolCallID: "call_1|fc_1", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: "file body"}}, Timestamp: 4},
		ai.NewSystemText("later update", 5), ai.NewUserText("second user", 6))
	user := map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "first user"}}}
	for _, tc := range []struct {
		name         string
		messages     []ai.Message
		compat       json.RawMessage
		instructions string
		input        []any
	}{
		{name: "initial", messages: base, instructions: "root instructions", input: []any{user}},
		{name: "continued with later system", messages: continued, compat: json.RawMessage(`{"supportsMidConvoSystemMessages":true}`), instructions: "root instructions", input: []any{
			user,
			map[string]any{"type": "function_call", "call_id": "call_1", "id": "fc_1", "name": "read", "arguments": `{"path":"/x"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "file body"},
			map[string]any{"role": "developer", "content": "later update"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "second user"}}},
		}},
		{name: "empty leading system", messages: []ai.Message{ai.NewSystemText("", 1), ai.NewUserText("first user", 2)}, instructions: "You are a helpful assistant.", input: []any{user}},
		{name: "no system", messages: []ai.Message{ai.NewUserText("first user", 2)}, instructions: "You are a helpful assistant.", input: []any{user}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := codexFixtureModel("https://fixture.invalid")
			model.Compat = tc.compat
			opts := codexFixtureOptions()
			calls := 0
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Scheme != "https" || r.URL.Host != "fixture.invalid" || r.URL.Path != "/codex/responses" || r.URL.RawQuery != "" {
					t.Error("wrong synthetic endpoint")
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload["instructions"] != tc.instructions {
					t.Error("instructions mismatch")
				}
				if !reflect.DeepEqual(payload["input"], tc.input) {
					t.Error("input shape mismatch")
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":"synthetic"}`)), Header: make(http.Header)}, nil
			}))
			stream := ai.StreamSimple(context.Background(), model, ai.Context{Messages: tc.messages}, opts)
			var events []ai.AssistantMessageEvent
			for event := range stream.Events() {
				events = append(events, event)
			}
			if calls != 1 || len(events) != 1 || events[0].Type != ai.EventError || stream.Result().StopReason != ai.StopError {
				t.Error("synthetic 400 must be single terminal failure without replay")
			}
		})
	}
}
