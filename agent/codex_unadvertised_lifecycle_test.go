package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestCodexPostPayloadToolAuthorizationLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, response            string
		remove, doneOnly, execute bool
	}{
		{"removed read both events", "read", true, false, false},
		{"removed read done name", "read", true, true, false},
		{"advertised read", "read", false, false, true},
		{"unknown continuation", "not_a_tool", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, executions := 0, 0
			var replayName string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var body struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
					Input []map[string]any `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if tc.remove && len(body.Tools) != 0 {
					t.Errorf("removed tools advertised: %#v", body.Tools)
				}
				if !tc.remove && (len(body.Tools) != 1 || body.Tools[0].Name != "read") {
					t.Errorf("advertised tools: %#v", body.Tools)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if requests == 1 {
					added := tc.response
					if tc.doneOnly {
						added = ""
					}
					fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc\",\"call_id\":\"call\",\"name\":%q}}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc\",\"call_id\":\"call\",\"name\":%q,\"arguments\":\"{}\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", added, tc.response)
				} else {
					for _, item := range body.Input {
						if item["type"] == "function_call" && item["call_id"] == "call" {
							replayName, _ = item["name"].(string)
						}
					}
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
				}
			}))
			defer server.Close()
			model := *ai.GetModel("openai-codex", "gpt-6-sol")
			model.BaseURL = server.URL
			a := NewAgent(AgentOptions{InitialState: &AgentState{Model: &model, Tools: []AgentTool{{Name: "read", Parameters: ai.Object(), Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
				executions++
				return AgentToolResult{}, nil
			}}}}, HTTPClient: server.Client(), CodexAuth: func(context.Context) (ai.ModelAuth, error) {
				return ai.ModelAuth{APIKey: "fixture", Headers: ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture")}}, nil
			}, OnPayload: func(payload any, _ *ai.Model) (any, error) {
				if tc.remove {
					delete(payload.(map[string]any), "tools")
				}
				return nil, nil
			}})
			if err := a.Prompt(context.Background(), "test"); err != nil {
				t.Fatal(err)
			}
			if requests != 2 || (executions == 1) != tc.execute || a.State().ErrorMessage != "" {
				t.Fatalf("requests=%d executions=%d error=%q", requests, executions, a.State().ErrorMessage)
			}
			var result *ai.ToolResultMessage
			for _, m := range a.State().Messages {
				if r, ok := m.(ai.ToolResultMessage); ok && r.ToolCallID == "call|fc" {
					rr := r
					result = &rr
				}
			}
			if result == nil || result.IsError == tc.execute {
				t.Fatalf("result: %#v", result)
			}
			if tc.remove {
				if result.ToolName == "read" || !strings.HasPrefix(result.ToolName, "unknown_") {
					t.Fatalf("not isolated: %#v", result)
				}
			} else if result.ToolName != tc.response {
				t.Fatalf("name changed: %#v", result)
			}
			if replayName != result.ToolName {
				t.Fatalf("replay=%q result=%q", replayName, result.ToolName)
			}
		})
	}
}
