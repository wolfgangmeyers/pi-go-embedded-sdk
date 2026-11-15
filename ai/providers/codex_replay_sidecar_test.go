package providers

import (
	"reflect"
	"strings"
	"testing"
)

func replayCall(id, name, args string) map[string]any {
	return map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": args}
}
func TestCodexReplayMatchHooks(t *testing.T) {
	a, b := replayCall("a", "tool", `{"x":1}`), replayCall("b", "tool", `{}`)
	original := []codexReplayCall{codexReplaySnapshot(a, "advertised"), codexReplaySnapshot(b, "unadvertised")}
	tests := []struct {
		name  string
		input []any
		want  []string
	}{
		{"reordered", []any{b, a}, []string{"unadvertised", "advertised"}},
		{"removed", []any{b}, []string{"unadvertised"}},
		{"duplicated", []any{a, a, b}, []string{"", "", "unadvertised"}},
		{"changed args", []any{replayCall("a", "tool", `{"x":2}`), b}, []string{"", "unadvertised"}},
		{"duplicate id changed identity", []any{a, replayCall("a", "other", `{}`)}, []string{"", ""}},
		{"missing identity", []any{map[string]any{"type": "function_call", "call_id": "a"}, a}, []string{"", ""}},
		{"inserted", []any{replayCall("c", "tool", `{}`), a}, []string{"", "advertised"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexReplayMatch(original, tt.input); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
	a["arguments"] = "changed"
	if got := codexReplayMatch(original, []any{a}); got[0] != "" {
		t.Fatal("snapshot was mutated")
	}
}
func TestCodexReplayIsolation(t *testing.T) {
	input := []any{replayCall("a", strings.Repeat("hostile!", 100), `secret`), replayCall("b", strings.Repeat("hostile!", 100), `secret`)}
	first, err := codexReplayIsolatedNames(input, nil, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if first[0] == first[1] {
		t.Fatal("distinct calls collided")
	}
	for _, name := range first {
		if !codexValidHistoryName.MatchString(name) || strings.Contains(name, "secret") {
			t.Fatal(name)
		}
	}
	occupied := map[string]bool{first[0]: true}
	input = append(input, replayCall("c", first[1], `{}`))
	second, err := codexReplayIsolatedNames(input, occupied, []int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	again, err := codexReplayIsolatedNames(input, occupied, []int{0, 1})
	if err != nil || !reflect.DeepEqual(second, again) {
		t.Fatal(second, again, err)
	}
	if second[0] == first[0] || second[1] == first[1] || second[0] == second[1] {
		t.Fatal(second)
	}
	if _, err = codexReplayIsolatedNames(input, nil, []int{0, 0}); err == nil {
		t.Fatal("duplicate position accepted")
	}
	if _, err = codexReplayIsolatedNames([]any{replayCall("", "x", `{}`)}, nil, []int{0}); err == nil {
		t.Fatal("missing identity accepted")
	}
}

func TestCodexReplayInvalidDuplicateIDs(t *testing.T) {
	valid := replayCall("same", "tool", "{}")
	invalid := []map[string]any{
		{"type": "function_call", "call_id": "same"},
		{"type": "custom_tool_call", "call_id": "same", "name": "tool", "bad": make(chan int)},
	}
	for _, bad := range invalid {
		original := []codexReplayCall{codexReplaySnapshot(valid, "advertised"), codexReplaySnapshot(bad, "ignored")}
		if original[1].CallID != "same" || original[1].Valid {
			t.Fatal("invalid call lost its ID or was accepted")
		}
		if got := codexReplayMatch(original, []any{valid}); !reflect.DeepEqual(got, []string{""}) {
			t.Fatalf("original duplicate: %v", got)
		}
		if got := codexReplayMatch(original[:1], []any{valid, bad}); !reflect.DeepEqual(got, []string{"", ""}) {
			t.Fatalf("output duplicate: %v", got)
		}
	}
}

func TestCodexReplayLargePayloadDigest(t *testing.T) {
	call := replayCall("large", "tool", strings.Repeat("sensitive", 1<<18))
	snapshot := codexReplaySnapshot(call, "advertised")
	if !snapshot.Valid || len(snapshot.Identity) != 32 {
		t.Fatal("expected fixed-size digest")
	}
	if got := codexReplayMatch([]codexReplayCall{snapshot}, []any{call}); got[0] != "advertised" {
		t.Fatal("large call did not match")
	}
	call["arguments"] = strings.Repeat("sensitive", 1<<18) + "x"
	if got := codexReplayMatch([]codexReplayCall{snapshot}, []any{call}); got[0] != "" {
		t.Fatal("changed large call matched")
	}
}

func TestCodexReplayIsolationReservesWireAlias(t *testing.T) {
	input := []any{replayCall("a", "tool", "{}")}
	initial, err := codexReplayIsolatedNames(input, nil, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	// A generated alias may not appear in the history yet; the caller reserves it.
	alias := initial[0]
	names, err := codexReplayIsolatedNames(input, map[string]bool{alias: true}, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	if names[0] == alias || names[0] != alias+"_1" {
		t.Fatalf("alias collision: %v", names)
	}
}
