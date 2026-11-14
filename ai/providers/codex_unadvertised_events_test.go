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

func TestCodexRemovedOrdinaryToolStreamEvents(t *testing.T) {
	for _, doneOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(doneOnly), func(t *testing.T) {
			opts := codexFixtureOptions()
			opts.OnPayload = func(payload any, _ *ai.Model) (any, error) {
				delete(payload.(map[string]any), "tools")
				return nil, nil
			}
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(*http.Request) (*http.Response, error) {
				name := "read"
				if doneOnly {
					name = ""
				}
				events := fmt.Sprintf("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc\",\"call_id\":\"call\",\"name\":%q}}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc\",\"call_id\":\"call\",\"name\":\"read\",\"arguments\":\"{}\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", name)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(events))}, nil
			}))
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("test", 1)}, Tools: []ai.Tool{{Name: "read", Parameters: ai.Object()}}}
			stream := ai.StreamSimple(context.Background(), codexFixtureModel("https://fixture.invalid"), req, opts)
			seen := map[ai.EventType]string{}
			for event := range stream.Events() {
				if event.Type == ai.EventToolCallStart || event.Type == ai.EventToolCallEnd {
					call := event.Partial.Content[event.ContentIndex].(ai.ToolCall)
					seen[event.Type] = call.Name
				}
			}
			result := stream.Result()
			if result.StopReason != ai.StopToolUse {
				t.Fatalf("result: %#v", result)
			}
			start, end := seen[ai.EventToolCallStart], seen[ai.EventToolCallEnd]
			if !strings.HasPrefix(start, "unknown_") || start != end || result.Content[0].(ai.ToolCall).Name != start {
				t.Fatalf("events=%v result=%#v", seen, result)
			}
		})
	}
}
