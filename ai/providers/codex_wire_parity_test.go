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

// Local synthetic doer only: no network, credentials, or captured raw wire data.
func TestCodexSSEWireParityOffline(t *testing.T) {
	for _, tc := range []struct {
		name         string
		instructions string
		tool         bool
		status       int
		partial      bool
		choice       ai.ToolChoice
		reasoning    ai.ThinkingLevel
		wantEffort   string
		wantSummary  bool
	}{
		{name: "leading system", instructions: "synthetic system", wantEffort: "none"},
		{name: "empty fallback", wantEffort: "none"},
		{name: "ordinary function", instructions: "synthetic system", tool: true, wantEffort: "none"},
		{name: "explicit off", reasoning: "off", wantEffort: "none"},
		{name: "explicit medium", reasoning: ai.ThinkingMedium, wantEffort: "medium", wantSummary: true},
		{name: "explicit tool choice", instructions: "synthetic system", choice: ai.ToolChoiceNone, wantEffort: "none"},
		{name: "preoutput 401", instructions: "synthetic system", status: http.StatusUnauthorized, wantEffort: "none"},
		{name: "partial unauthorized", instructions: "synthetic system", partial: true, wantEffort: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			model := codexFixtureModel("https://fixture.invalid")
			opts := codexFixtureOptions()
			opts.MaxRetries = 3
			opts.ToolChoice = tc.choice
			opts.Reasoning = tc.reasoning
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "fixture.invalid" || r.URL.Path != "/codex/responses" || r.URL.RawQuery != "" {
					t.Error("Codex endpoint mismatch")
				}
				if len(r.Header) != 7 || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("ChatGPT-Account-ID") != "fixture-account" || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Originator") != "pi" || r.Header.Get("User-Agent") != piUserAgent() || r.Header.Get("OpenAI-Beta") != "responses=experimental" {
					t.Error("Codex fixed header contract mismatch")
				}
				for _, name := range []string{"Authorization", "Chatgpt-Account-Id", "Accept", "Content-Type", "Originator", "User-Agent", "Openai-Beta"} {
					if _, present := r.Header[name]; !present {
						t.Error("Codex header name missing")
					}
				}
				var body map[string]any
				if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&body); err != nil {
					t.Error("synthetic payload decoding failed")
				} else {
					wantKeys := []string{"include", "input", "instructions", "model", "parallel_tool_calls", "reasoning", "store", "stream", "text", "tool_choice"}
					if tc.tool {
						wantKeys = append(wantKeys, "tools")
					}
					if len(body) != len(wantKeys) {
						t.Error("Codex body key count mismatch")
					}
					for _, key := range wantKeys {
						if _, ok := body[key]; !ok {
							t.Error("Codex body key missing")
						}
					}
					wantInstructions := tc.instructions
					if wantInstructions == "" {
						wantInstructions = "You are a helpful assistant."
					}
					wantChoice := ai.ToolChoiceAuto
					if tc.choice != "" {
						wantChoice = tc.choice
					}
					if body["model"] != "gpt-6-sol" || body["instructions"] != wantInstructions || body["stream"] != true || body["store"] != false || body["tool_choice"] != string(wantChoice) || body["parallel_tool_calls"] != true || !reflect.DeepEqual(body["text"], map[string]any{"verbosity": "low"}) || !reflect.DeepEqual(body["include"], []any{"reasoning.encrypted_content"}) {
						t.Error("Codex body defaults mismatch")
					}
					wantReasoning := map[string]any{"effort": tc.wantEffort}
					if tc.wantSummary {
						wantReasoning["summary"] = "auto"
					}
					if !reflect.DeepEqual(body["reasoning"], wantReasoning) {
						t.Error("Codex reasoning effort or summary mismatch")
					}
					input, ok := body["input"].([]any)
					if !ok || len(input) != 1 {
						t.Error("Codex input count mismatch")
					} else if item, ok := input[0].(map[string]any); !ok || item["role"] != "user" {
						t.Error("Codex input role mismatch")
					}
					if tc.tool {
						tools, ok := body["tools"].([]any)
						if !ok || len(tools) != 1 {
							t.Error("Codex tool count mismatch")
						} else if tool, ok := tools[0].(map[string]any); !ok || tool["type"] != "function" || tool["name"] != "synthetic_tool" || !reflect.DeepEqual(tool["strict"], nil) {
							t.Error("Codex ordinary function strict:null mismatch")
						} else if _, present := tool["strict"]; !present {
							t.Error("Codex ordinary function strict absent")
						}
					}
				}
				status := http.StatusOK
				response := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
				if tc.status != 0 {
					status = tc.status
					response = ""
				}
				if tc.partial {
					response = "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"synthetic\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"synthetic partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_token\"}}}\n\n"
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
			}))
			req := ai.Context{SystemPrompt: tc.instructions, Messages: []ai.Message{ai.NewUserText("synthetic user", 2)}}
			if tc.tool {
				req.Tools = []ai.Tool{{Name: "synthetic_tool", Description: "synthetic", Parameters: &ai.Schema{Type: "object"}}}
			}
			stream := ai.StreamSimple(context.Background(), model, req, opts)
			var events []ai.EventType
			for event := range stream.Events() {
				events = append(events, event.Type)
			}
			final := stream.Result()
			if calls != 1 {
				t.Error("Codex adapter replayed request")
			}
			if tc.status == http.StatusUnauthorized {
				if final.StopReason != ai.StopError || !reflect.DeepEqual(events, []ai.EventType{ai.EventError}) {
					t.Error("preoutput 401 terminal mismatch")
				}
			} else if tc.partial {
				if final.StopReason != ai.StopError || len(events) == 0 || events[len(events)-1] != ai.EventError {
					t.Error("partial stream outcome mismatch")
				}
				for _, ev := range events {
					if ev == ai.EventDone {
						t.Error("partial stream fabricated completion")
					}
				}
			} else if final.StopReason != ai.StopStop || len(events) == 0 || events[len(events)-1] != ai.EventDone {
				t.Error("completed stream outcome mismatch")
			}
		})
	}
}
