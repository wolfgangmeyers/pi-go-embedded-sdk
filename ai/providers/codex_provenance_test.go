package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestCodexStreamProvenance(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		late, removed, advertised bool
	}{
		{"mecha_worker_launch", false, false, true}, {"mecha_worker_launch", true, false, true},
		{"unlisted", false, false, false}, {"unlisted", true, false, false},
		{"mecha_worker.launch", false, false, false}, {"mecha_worker_launch", true, true, false},
	} {
		t.Run(fmt.Sprintf("%s-late=%v-removed=%v", tc.name, tc.late, tc.removed), func(t *testing.T) {
			opts := codexFixtureOptions()
			if tc.removed {
				opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
					delete(payload.(map[string]any), "tools")
					return nil, nil
				}
			}
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(*http.Request) (*http.Response, error) {
				early := tc.name
				if tc.late {
					early = ""
				}
				events := []string{fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call","name":%q}}`, early),
					`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
					fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call","name":%q,"arguments":"{}"}}`, tc.name),
					`{"type":"response.completed","response":{"status":"completed"}}`}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + strings.Join(events, "\n\ndata: ") + "\n\n"))}, nil
			}))
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("test", 1)}, Tools: []ai.Tool{{Name: "mecha_worker.launch", Parameters: ai.Object()}}}
			stream := ai.StreamSimple(context.Background(), codexFixtureModel("https://fixture.invalid"), req, opts)
			count := 0
			check := func(call ai.ToolCall) {
				t.Helper()
				p := call.CodexReplayProvenance
				if p == nil || p.Version != 1 || p.Advertised != tc.advertised {
					t.Fatalf("call %#v", call)
				}
			}
			for e := range stream.Events() {
				if e.Type == ai.EventToolCallStart || e.Type == ai.EventToolCallDelta || e.Type == ai.EventToolCallEnd {
					count++
					check(e.Partial.Content[e.ContentIndex].(ai.ToolCall))
					if e.ToolCall != nil {
						check(*e.ToolCall)
					}
				}
			}
			result := stream.Result()
			if result.StopReason != ai.StopToolUse || count < 2 {
				t.Fatalf("result %#v count %d", result, count)
			}
			check(result.Content[0].(ai.ToolCall))
		})
	}
}
