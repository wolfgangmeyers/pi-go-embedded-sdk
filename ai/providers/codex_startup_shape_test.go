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

// Characterization only: the installed Pi Codex adapter is a local comparison,
// not evidence of the upstream /codex/responses validation contract or a 400 cause.
func TestCodexEmbeddedRootStartupShapeSynthetic400(t *testing.T) {
	const secret = "fake-g7-bearer-sentinel"
	// Installed Pi buildRequestBody sets prompt_cache_key to undefined without
	// a session ID; JSON.stringify omits it. These are the serialized fields
	// shared by the installed Pi fixture and this SDK's default request.
	piCore := []string{"include", "input", "instructions", "model", "parallel_tool_calls", "store", "stream", "text", "tool_choice"}
	for _, tc := range []struct {
		name, compat string
		tool         bool
		wantStrict   bool
	}{
		{name: "no registered tools"},
		{name: "registered function tool", tool: true, wantStrict: true},
		{name: "strict-capable registered function tool", tool: true, compat: `{"supportsStrictMode":true}`, wantStrict: true},
		{name: "strict-disabled registered function tool", tool: true, compat: `{"supportsStrictMode":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := codexFixtureModel("https://fixture.invalid")
			if tc.compat != "" {
				model.Compat = json.RawMessage(tc.compat)
			}
			ctx := ai.Context{SystemPrompt: "synthetic root startup instructions", Messages: []ai.Message{
				ai.NewUserText("synthetic initial instruction", 2),
			}}
			if tc.tool {
				ctx.Tools = []ai.Tool{{Name: "read", Description: "read a file", Parameters: ai.Object(ai.Prop("path", ai.String()))}}
			}
			attempts := 0
			var captured map[string]any
			opts := codexFixtureOptions()
			opts.MaxRetries = 4
			opts.CodexAuth = func(context.Context) (ai.ModelAuth, error) {
				return ai.ModelAuth{APIKey: secret, Headers: ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture-account")}}, nil
			}
			// HTTPDoer intercepts the request before any socket or real issuer exists.
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(r *http.Request) (*http.Response, error) {
				attempts++
				if r.URL.Scheme != "https" || r.URL.Host != "fixture.invalid" || r.URL.Path != "/codex/responses" || r.Method != http.MethodPost {
					t.Error("synthetic endpoint or method mismatch")
				}
				if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("synthetic headers mismatch")
				}
				if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
					t.Error("invalid generated JSON request")
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"synthetic_bad_request","message":"` + secret + `"}}`)), Header: make(http.Header)}, nil
			}))
			stream := ai.StreamSimple(context.Background(), model, ctx, opts)
			var events []ai.AssistantMessageEvent
			for ev := range stream.Events() {
				events = append(events, ev)
			}
			result := stream.Result()
			if attempts != 1 || len(events) != 1 || events[0].Type != ai.EventError || result == nil || result.StopReason != ai.StopError {
				t.Fatal("synthetic 400 must yield one attempt and one terminal error")
			}
			for _, ev := range events {
				if ev.Type == ai.EventDone || ev.Type == ai.EventStart {
					t.Fatal("synthetic HTTP error produced a checked final or stream start")
				}
			}
			for _, public := range []any{events, result, result.Diagnostics} {
				encoded, err := json.Marshal(public)
				if err != nil || strings.Contains(string(encoded), secret) {
					t.Fatal("synthetic secret reached public result")
				}
			}
			if captured == nil {
				t.Fatal("request never decoded")
			}
			if captured["model"] != "gpt-6-sol" || captured["stream"] != true || captured["store"] != false || !reflect.DeepEqual(captured["include"], []any{"reasoning.encrypted_content"}) {
				t.Fatal("model, streaming or include fields mismatch")
			}
			input, ok := captured["input"].([]any)
			if captured["instructions"] != "synthetic root startup instructions" {
				t.Fatalf("startup instructions mismatch: %q", captured["instructions"])
			}
			if !ok || len(input) != 1 {
				t.Fatal("startup input count/type mismatch")
			}
			user, ok := input[0].(map[string]any)
			if !ok || user["role"] != "user" {
				t.Fatal("startup user role mismatch")
			}
			content, ok := user["content"].([]any)
			if !ok || len(content) != 1 || !reflect.DeepEqual(content[0], map[string]any{"type": "input_text", "text": "synthetic initial instruction"}) {
				t.Fatal("startup user content mismatch")
			}
			tools, hasTools := captured["tools"]
			if hasTools != tc.tool {
				t.Fatal("optional tool presence mismatch")
			}
			if hasTools {
				list, ok := tools.([]any)
				if !ok || len(list) != 1 {
					t.Fatal("registered tool count/type mismatch")
				}
				tool, ok := list[0].(map[string]any)
				if !ok || tool["type"] != "function" || tool["name"] != "read" || tool["description"] != "read a file" {
					t.Fatal("registered function tool mismatch")
				}
				params, ok := tool["parameters"].(map[string]any)
				if !ok || params["type"] != "object" {
					t.Fatal("registered tool schema mismatch")
				}
				props, ok := params["properties"].(map[string]any)
				if !ok || !reflect.DeepEqual(props["path"], map[string]any{"type": "string"}) {
					t.Fatal("registered tool property mismatch")
				}
				strict, present := tool["strict"]
				// Codex's ordinary unconstrained function sends explicit strict:null
				// when supported; disabling strict support omits the field.
				if present != tc.wantStrict || (present && strict != nil) {
					t.Fatal("strict field presence/type mismatch")
				}
			}
			// Verify the generated body keys while keeping Pi-local shape as
			// characterization, not an explanation of an upstream HTTP 400.
			wantSDK := []string{"include", "input", "instructions", "model", "parallel_tool_calls", "reasoning", "store", "stream", "text", "tool_choice"}
			if tc.tool {
				wantSDK = append(wantSDK, "tools")
			}
			gotKeys := make(map[string]bool, len(captured))
			for key := range captured {
				gotKeys[key] = true
			}
			wantKeys := make(map[string]bool, len(wantSDK))
			for _, key := range wantSDK {
				wantKeys[key] = true
			}
			if !reflect.DeepEqual(gotKeys, wantKeys) {
				t.Fatal("generated top-level field set changed")
			}
			for _, key := range piCore {
				_, present := captured[key]
				if !present {
					t.Fatalf("shared serialized field %s characterization changed", key)
				}
			}
			if _, present := captured["prompt_cache_key"]; present {
				t.Fatal("sessionless SDK request unexpectedly serialized prompt_cache_key")
			}
			if captured["tool_choice"] != "auto" || captured["parallel_tool_calls"] != true || !reflect.DeepEqual(captured["text"], map[string]any{"verbosity": "low"}) || !reflect.DeepEqual(captured["reasoning"], map[string]any{"effort": "none"}) {
				t.Fatal("startup choice, parallel, text or reasoning default mismatch")
			}
		})
	}
}

type codexFakeDoer func(*http.Request) (*http.Response, error)

func (f codexFakeDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }
