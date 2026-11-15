package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestCodexToolEndProvenanceIsIndependent(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("late-name=%v", late), func(t *testing.T) {
			opts := codexFixtureOptions()
			opts.HTTPClient = ai.HTTPDoer(codexFakeDoer(func(*http.Request) (*http.Response, error) {
				name := "tool"
				if late {
					name = ""
				}
				events := []string{
					fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call","name":%q}}`, name),
					`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call","name":"tool","arguments":"{}"}}`,
					`{"type":"response.completed","response":{"status":"completed"}}`,
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + strings.Join(events, "\n\ndata: ") + "\n\n"))}, nil
			}))
			req := ai.Context{Messages: []ai.Message{ai.NewUserText("test", 1)}, Tools: []ai.Tool{{Name: "tool", Parameters: ai.Object()}}}
			stream := ai.StreamSimple(context.Background(), codexFixtureModel("https://fixture.invalid"), req, opts)
			var end ai.AssistantMessageEvent
			for event := range stream.Events() {
				if event.Type == ai.EventToolCallEnd {
					end = event
				}
			}
			result := stream.Result()
			if end.ToolCall == nil || end.ToolCall.CodexReplayProvenance == nil || result.StopReason != ai.StopToolUse || len(result.Content) != 1 {
				t.Fatalf("end=%#v result=%#v", end, result)
			}
			final := result.Content[0].(ai.ToolCall).CodexReplayProvenance
			partial := end.Partial.Content[0].(ai.ToolCall).CodexReplayProvenance
			if final == nil || partial == nil || !final.Advertised || !partial.Advertised {
				t.Fatalf("final=%#v partial=%#v", final, partial)
			}
			end.ToolCall.CodexReplayProvenance.Advertised = false
			if !final.Advertised || !partial.Advertised {
				t.Fatal("mutating event tool call changed final or partial provenance")
			}
			// Exercise concurrent consumer mutation and final-result reads under -race.
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for i := 0; i < 10000; i++ {
					end.ToolCall.CodexReplayProvenance.Advertised = i%2 == 0
				}
			}()
			go func() {
				defer wg.Done()
				for i := 0; i < 10000; i++ {
					_ = final.Advertised
				}
			}()
			wg.Wait()
		})
	}
}
