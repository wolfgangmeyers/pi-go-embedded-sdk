package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexProvenanceJSON(t *testing.T) {
	for _, tc := range []struct {
		input             string
		known, advertised bool
	}{
		{`{"version":1,"advertised":true}`, true, true}, {`{"version":1,"advertised":false}`, true, false},
		{`null`, false, false}, {`{}`, false, false}, {`{"version":2,"advertised":true}`, false, false},
		{`{"version":"1","advertised":true}`, false, false}, {`{"version":1}`, false, false}, {`{"version":1,"advertised":"false"}`, false, false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			var calls ContentList
			if err := json.Unmarshal([]byte(`[{"type":"toolCall","id":"id","name":"read","arguments":{"z":1,"a":2},"codexReplayProvenance":`+tc.input+`}]`), &calls); err != nil {
				t.Fatal(err)
			}
			call := calls[0].(ToolCall)
			p := call.CodexReplayProvenance
			if (p != nil) != tc.known || (p != nil && p.Advertised != tc.advertised) {
				t.Fatalf("provenance: %#v", p)
			}
			data, err := json.Marshal(calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "codexReplayProvenance") != tc.known || !strings.Contains(string(data), `"arguments":{"z":1,"a":2}`) {
				t.Fatalf("encoded %s", data)
			}
			var round ContentList
			if err := json.Unmarshal(data, &round); err != nil {
				t.Fatal(err)
			}
			if (round[0].(ToolCall).CodexReplayProvenance != nil) != tc.known {
				t.Fatal(string(data))
			}
		})
	}
	var legacy ToolCall
	if err := json.Unmarshal([]byte(`{"id":"a","name":"b","arguments":{}}`), &legacy); err != nil || legacy.CodexReplayProvenance != nil {
		t.Fatal(legacy, err)
	}
}

func TestCodexProvenanceClone(t *testing.T) {
	original := &AssistantMessage{Content: ContentList{ToolCall{CodexReplayProvenance: &CodexReplayProvenance{Version: 1, Advertised: true}}}}
	clone := original.Clone()
	clone.Content[0].(ToolCall).CodexReplayProvenance.Advertised = false
	if !original.Content[0].(ToolCall).CodexReplayProvenance.Advertised {
		t.Fatal("clone shares provenance")
	}
}
