package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-valley/pi/ai"
	_ "github.com/sky-valley/pi/ai/providers"
)

func TestCodexAliasExhaustionHistoryLifecycle(t *testing.T) {
	var history []AgentMessage
	for i := 0; i < 32; i++ {
		name := "mecha_worker_launch"
		if i > 0 {
			name = fmt.Sprintf("mecha_worker_launch_%d", i)
		}
		id := fmt.Sprintf("call%d|fc%d", i, i)
		history = append(history, ai.AssistantMessage{Api: ai.APIOpenAICodexResponses, Provider: "openai-codex", Model: "gpt-6-sol", StopReason: ai.StopToolUse, Content: ai.ContentList{ai.ToolCall{ID: id, Name: name, Arguments: map[string]any{}}}}, ai.ToolResultMessage{ToolCallID: id, ToolName: name, IsError: true, Content: ai.ContentList{ai.TextContent{Text: "unavailable"}}})
	}
	requests := 0
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
		if len(body.Tools) != 1 {
			t.Errorf("tools: %#v", body.Tools)
			return
		}
		alias := body.Tools[0].Name
		found := map[string]bool{}
		for _, item := range body.Input {
			if item["type"] == "function_call" {
				name, _ := item["name"].(string)
				found[name] = true
				if name == alias {
					t.Errorf("historical unknown authorized as %q", alias)
				}
			}
		}
		for i := 0; i < 32; i++ {
			name := "mecha_worker_launch"
			if i > 0 {
				name = fmt.Sprintf("mecha_worker_launch_%d", i)
			}
			if !found[name] {
				t.Errorf("missing history %s", name)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_new\",\"call_id\":\"call_new\",\"name\":\"mecha_worker_launch_31\"}}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_new\",\"call_id\":\"call_new\",\"name\":\"mecha_worker_launch_31\",\"arguments\":\"{}\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"done\"}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		}
	}))
	defer server.Close()
	model := *ai.GetModel("openai-codex", "gpt-6-sol")
	model.BaseURL = server.URL
	executed := 0
	a := NewAgent(AgentOptions{InitialState: &AgentState{Model: &model, Messages: history, Tools: []AgentTool{{Name: "mecha_worker.launch", Parameters: &ai.Schema{Type: "object"}, Execute: func(context.Context, string, map[string]any, ToolUpdateFunc) (AgentToolResult, error) {
		executed++
		return AgentToolResult{}, nil
	}}}}, HTTPClient: server.Client(), CodexAuth: func(context.Context) (ai.ModelAuth, error) {
		return ai.ModelAuth{APIKey: "fixture", Headers: ai.ProviderHeaders{"ChatGPT-Account-ID": ai.HeaderValue("fixture")}}, nil
	}})
	if err := a.Prompt(context.Background(), "continue"); err != nil {
		t.Fatal(err)
	}
	state := a.State()
	if requests != 2 || executed != 0 || state.ErrorMessage != "" {
		t.Fatalf("requests=%d executed=%d error=%q", requests, executed, state.ErrorMessage)
	}
	rejected := false
	for _, msg := range state.Messages {
		if result, ok := msg.(ai.ToolResultMessage); ok && result.ToolCallID == "call_new|fc_new" && result.IsError {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("unknown call not rejected")
	}
}
