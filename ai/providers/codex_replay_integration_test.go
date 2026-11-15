package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestCodexReplayIsolationPayloadHooks(t *testing.T) {
	for _, mode := range []string{"unchanged", "reorder", "duplicate", "mutate", "insert", "remove", "legacy", "advertised"} {
		t.Run(mode, func(t *testing.T) {
			advertised := mode == "advertised" || mode == "reorder" || mode == "duplicate" || mode == "mutate" || mode == "remove"
			p := &ai.CodexReplayProvenance{Version: 1, Advertised: advertised}
			if mode == "legacy" {
				p = nil
			}
			model := codexFixtureModel("https://fixture.invalid")
			msg := ai.AssistantMessage{Api: model.Api, Provider: model.Provider, Model: model.ID, Content: ai.ContentList{ai.ToolCall{ID: "call_1|fc_1", Name: "mecha_worker.launch", Arguments: map[string]any{}, CodexReplayProvenance: p}}, StopReason: ai.StopToolUse}
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("test", 1), msg, ai.ToolResultMessage{ToolCallID: "call_1|fc_1", ToolName: "mecha_worker.launch", Content: ai.ContentList{ai.TextContent{Text: "safe result"}}, IsError: true}}, Tools: []ai.Tool{{Name: "mecha_worker.launch", Parameters: ai.Object()}}}
			opts := codexFixtureOptions()
			opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
				m := payload.(map[string]any)
				input := m["input"].([]any)
				var index int
				for i, v := range input {
					if x, ok := v.(map[string]any); ok && x["type"] == "function_call" {
						index = i
						break
					}
				}
				switch mode {
				case "reorder":
					input[0], input[index] = input[index], input[0]
				case "duplicate":
					copyCall := map[string]any{}
					for k, v := range input[index].(map[string]any) {
						copyCall[k] = v
					}
					input = append(input, copyCall)
				case "mutate":
					input[index].(map[string]any)["arguments"] = `{"changed":true}`
				case "insert":
					input = append(input, map[string]any{"type": "function_call", "call_id": "inserted", "name": "mecha_worker.launch", "arguments": "{}"})
				case "remove":
					input = append(input[:index], input[index+1:]...)
				}
				m["input"] = input
				return nil, nil
			}
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(r *http.Request) (*http.Response, error) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				alias := body["tools"].([]any)[0].(map[string]any)["name"].(string)
				calls := 0
				result := false
				for _, v := range body["input"].([]any) {
					x, ok := v.(map[string]any)
					if !ok {
						continue
					}
					if x["type"] == "function_call_output" && x["call_id"] == "call_1" && x["output"] == "safe result" {
						result = true
					}
					if x["type"] != "function_call" {
						continue
					}
					calls++
					name := x["name"].(string)
					shouldAlias := (mode == "advertised" || mode == "reorder") && x["call_id"] == "call_1"
					if (name == alias) != shouldAlias {
						t.Errorf("mode %s call %#v alias %s", mode, x, alias)
					}
				}
				if !result {
					t.Errorf("result missing: %#v", body)
				}
				if mode != "remove" && calls == 0 {
					t.Error("call missing")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
			}))
			stream := ai.StreamSimple(context.Background(), model, req, opts)
			for range stream.Events() {
			}
			if got := stream.Result(); got.StopReason == ai.StopError {
				t.Fatal(fmt.Sprintf("%#v", got))
			}
		})
	}
}
