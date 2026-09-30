package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestCodexWireAliasCollisionHistoryAndChoice(t *testing.T) {
	system := ai.NewSystemText("synthetic", 1)
	system.ToolsAdded = []ai.Tool{{Name: "mecha_worker.launch"}, {Name: "mecha_worker_launch"}}
	historical := ai.NewSystemText("later", 2)
	historical.ToolsAdded = []ai.Tool{{Name: "mecha_worker_launch_1"}, {Name: "read"}}
	payload := map[string]any{
		"tools":       []map[string]any{{"type": "function", "name": "mecha_worker.launch", "description": "unchanged", "parameters": map[string]any{"type": "object"}}, {"type": "function", "name": "mecha_worker_launch"}},
		"input":       []any{map[string]any{"type": "function_call", "name": "mecha_worker.launch", "call_id": "original", "arguments": "{}"}, map[string]any{"type": "additional_tools", "tools": []map[string]any{{"type": "function", "name": "mecha_worker_launch_1"}, {"type": "function", "name": "read"}}}},
		"tool_choice": map[string]any{"type": "function", "name": "mecha_worker.launch"},
	}
	names, err := codexWireToolNames(payload, []ai.Message{system, historical}, []ai.Message{system, historical})
	if err != nil {
		t.Fatal(err)
	}
	if names["mecha_worker_launch_2"] != "mecha_worker.launch" || names["mecha_worker_launch"] != "mecha_worker_launch" {
		t.Fatalf("inverse names: %v", names)
	}
	wire := payload["tools"].([]map[string]any)
	input := payload["input"].([]any)
	if wire[0]["name"] != "mecha_worker_launch_2" || wire[0]["description"] != "unchanged" || !reflect.DeepEqual(wire[0]["parameters"], map[string]any{"type": "object"}) || input[0].(map[string]any)["name"] != "mecha_worker_launch_2" || input[0].(map[string]any)["call_id"] != "original" || payload["tool_choice"].(map[string]any)["name"] != "mecha_worker_launch_2" {
		t.Fatalf("wire alias mismatch: %#v", payload)
	}
	if system.ToolsAdded[0].Name != "mecha_worker.launch" {
		t.Fatal("transcript mutated")
	}
}

func TestCodexWireAliasRejectsHookDeclarations(t *testing.T) {
	system := ai.NewSystemText("synthetic", 1)
	system.ToolsAdded = []ai.Tool{{Name: "mecha_worker.launch"}}
	for _, payload := range []map[string]any{
		{"tools": []map[string]any{{"name": "mecha_worker.launch"}, {"name": "unexpected"}}},
		{"tools": []map[string]any{{"name": "mecha_worker.launch"}, {"name": "mecha_worker.launch"}}},
		{"input": []any{map[string]any{"type": "additional_tools", "tools": []map[string]any{{"name": "unexpected"}}}}},
	} {
		if _, err := codexWireToolNames(payload, []ai.Message{system}, []ai.Message{system}); err == nil {
			t.Fatalf("accepted hook declaration: %#v", payload)
		}
	}
}

// A's unnamed placeholder must not escape through B's partial snapshots.
func TestCodexWireAliasSSEPendingEarlierParallelPartial(t *testing.T) {
	for _, tc := range []struct{ name, lateName string }{
		{"resolved", "mecha_worker_launch"},
		{"undeclared", "undeclared_tool"},
		{"never_resolved", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := codexFixtureOptions()
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(r *http.Request) (*http.Response, error) {
				events := []string{
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a"}}`,
					`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"mecha_worker_launch"}}`,
					`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":1}"}`,
					`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"b\":2}"}`,
					`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"mecha_worker_launch","arguments":"{\"b\":2}"}}`,
					fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":%q,"arguments":"{\"a\":1}"}}`, tc.lateName),
					`{"type":"response.completed","response":{"status":"completed"}}`,
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + strings.Join(events, "\n\ndata: ") + "\n\n"))}, nil
			}))
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("synthetic", 1)}, Tools: []ai.Tool{{Name: "mecha_worker.launch", Parameters: &ai.Schema{Type: "object"}}}}
			stream := ai.StreamSimple(context.Background(), codexFixtureModel("https://fixture.invalid"), req, opts)
			var seen []ai.AssistantMessageEvent
			for event := range stream.Events() {
				seen = append(seen, event)
			}
			final := stream.Result()
			var sequence []string
			for _, event := range seen {
				if event.Partial != nil {
					for _, content := range event.Partial.Content {
						if call, ok := content.(ai.ToolCall); ok && call.Name != "mecha_worker.launch" {
							t.Fatalf("unnamed/aliased partial on %s: %#v", event.Type, event.Partial)
						}
					}
				}
				if event.Type == ai.EventToolCallStart || event.Type == ai.EventToolCallDelta || event.Type == ai.EventToolCallEnd {
					call := event.Partial.Content[event.ContentIndex].(ai.ToolCall)
					if call.Name != "mecha_worker.launch" {
						t.Fatalf("noncanonical tool event: %#v", event)
					}
					if event.Type == ai.EventToolCallEnd && (event.ToolCall == nil || event.ToolCall.Name != "mecha_worker.launch" || event.ToolCall.ID != call.ID) {
						t.Fatalf("invalid end: %#v", event)
					}
					sequence = append(sequence, fmt.Sprintf("%s:%s", event.Type, call.ID))
				}
			}
			if tc.name != "resolved" {
				if final.StopReason != ai.StopError || len(final.Content) != 0 || seen[len(seen)-1].Type != ai.EventError || len(seen[len(seen)-1].Error.Content) != 0 {
					t.Fatalf("unresolved call escaped: %#v %#v", final, seen)
				}
				if len(sequence) != 0 {
					t.Fatalf("unresolved events escaped: %v", sequence)
				}
				return
			}
			want := []string{"toolcall_start:call_b|fc_b", "toolcall_delta:call_b|fc_b", "toolcall_end:call_b|fc_b", "toolcall_start:call_a|fc_a", "toolcall_delta:call_a|fc_a", "toolcall_end:call_a|fc_a"}
			if !reflect.DeepEqual(sequence, want) || final.StopReason != ai.StopToolUse || seen[len(seen)-1].Type != ai.EventDone {
				t.Fatalf("sequence/result: %v %#v", sequence, final)
			}
		})
	}
}

// A completed response is final even if the SSE transport has more data.
func TestCodexWireAliasSSERejectsEventsAfterTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		late []string
	}{
		{"aliased_tool", []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_late","call_id":"call_late","name":"mecha_worker_launch"}}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_late","call_id":"call_late","name":"mecha_worker_launch","arguments":"{}"}}`,
		}},
		{"text", []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_late"}}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"late text"}`,
		}},
		{"second_terminal", []string{`{"type":"response.completed","response":{"status":"completed"}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var observed []string
			opts := codexFixtureOptions()
			opts.OnProviderStreamEvent = func(data any, _ *ai.Model) error {
				observed = append(observed, jsStringField(jsGet(data, "type")))
				return nil
			}
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				events := append([]string{`{"type":"response.completed","response":{"id":"resp_done","status":"completed"}}`}, tc.late...)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + strings.Join(events, "\n\ndata: ") + "\n\n"))}, nil
			}))
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("synthetic", 1)}, Tools: []ai.Tool{{Name: "mecha_worker.launch", Parameters: &ai.Schema{Type: "object"}}}}
			stream := ai.StreamSimple(context.Background(), codexFixtureModel("https://fixture.invalid"), req, opts)
			var seen []ai.AssistantMessageEvent
			for event := range stream.Events() {
				seen = append(seen, event)
			}
			final := stream.Result()
			if calls != 1 {
				t.Fatalf("replayed %d", calls)
			}
			if !reflect.DeepEqual(observed, []string{"response.completed"}) {
				t.Fatalf("late event reached hook: %v", observed)
			}
			if final.StopReason != ai.StopError || len(final.Content) != 0 || final.ResponseID != "resp_done" {
				t.Fatalf("late output escaped into result: %#v", final)
			}
			if len(seen) != 2 || seen[0].Type != ai.EventStart || seen[1].Type != ai.EventError || seen[1].Reason != ai.StopError || seen[1].Error == nil || len(seen[1].Error.Content) != 0 {
				t.Fatalf("late event emitted: %#v", seen)
			}
		})
	}
}

func TestCodexWireAliasSSECanonicalSplitLateParallelUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unknown bool
	}{{"canonical", false}, {"unknown", true}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			opts := codexFixtureOptions()
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				tools := body["tools"].([]any)
				if tools[0].(map[string]any)["name"] != "mecha_worker_launch" {
					t.Error("alias missing on wire")
				}
				name := "mecha_worker_launch"
				if tc.unknown {
					name = "undeclared_tool"
				}
				events := []string{
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_first","call_id":"call_first","name":"mecha_worker_launch"}}`,
					`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_late","call_id":"call_late"}}`,
					`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"n\":"}`,
					`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"m\":"}`,
					`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"1}"}`,
					`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"2}"}`,
					fmt.Sprintf(`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_late","call_id":"call_late","name":%q,"arguments":"{\"m\":2}"}}`, name),
					`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_first","call_id":"call_first","name":"mecha_worker_launch","arguments":"{\"n\":1}"}}`,
					`{"type":"response.completed","response":{"status":"completed"}}`,
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + strings.Join(events, "\n\ndata: ") + "\n\n"))}, nil
			}))
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("synthetic", 1)}, Tools: []ai.Tool{{Name: "mecha_worker.launch", Parameters: &ai.Schema{Type: "object"}}}}
			stream := ai.StreamSimple(context.Background(), codexFixtureModel("https://fixture.invalid"), req, opts)
			var seen []ai.AssistantMessageEvent
			for event := range stream.Events() {
				seen = append(seen, event)
			}
			final := stream.Result()
			if calls != 1 {
				t.Fatalf("replayed %d", calls)
			}
			if tc.unknown {
				if final.StopReason != ai.StopError || seen[len(seen)-1].Type != ai.EventError {
					t.Fatalf("unknown name accepted: %#v", final)
				}
				for _, event := range seen {
					if event.Type == ai.EventToolCallEnd || event.Type == ai.EventDone {
						t.Fatalf("unknown name emitted terminal call: %v", event.Type)
					}
				}
				return
			}
			if final.StopReason != ai.StopToolUse || len(final.Content) != 2 {
				t.Fatalf("result: %#v", final)
			}
			first := final.Content[0].(ai.ToolCall)
			late := final.Content[1].(ai.ToolCall)
			if first.Name != "mecha_worker.launch" || late.Name != "mecha_worker.launch" || first.ID != "call_first|fc_first" || late.ID != "call_late|fc_late" || first.Arguments["n"] != float64(1) || late.Arguments["m"] != float64(2) {
				t.Fatalf("calls: %#v %#v", first, late)
			}
			starts, deltas, ends := 0, 0, 0
			for _, event := range seen {
				if event.Type == ai.EventToolCallStart || event.Type == ai.EventToolCallDelta || event.Type == ai.EventToolCallEnd {
					call := event.Partial.Content[event.ContentIndex].(ai.ToolCall)
					if call.Name != "mecha_worker.launch" {
						t.Fatalf("aliased SDK event: %#v", event)
					}
					switch event.Type {
					case ai.EventToolCallStart:
						starts++
					case ai.EventToolCallDelta:
						deltas++
					case ai.EventToolCallEnd:
						ends++
						if event.ToolCall.Name != "mecha_worker.launch" {
							t.Fatal("aliased end")
						}
					}
				}
			}
			if starts != 2 || deltas != 4 || ends != 2 || seen[len(seen)-1].Type != ai.EventDone {
				t.Fatalf("event counts: %d %d %d", starts, deltas, ends)
			}
		})
	}
}
