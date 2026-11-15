package providers

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// codexReplayCall is captured before OnPayload. Identity is a fixed-size SHA-256
// digest of the serialized call, not the arguments themselves. Never log it or
// the call ID: digests can reveal low-entropy inputs through guessing. Retained
// identity storage is bounded per call (IDs and status remain variable-sized);
// serialization still temporarily allocates space proportional to the call.
// Status is deliberately a string until the persisted ToolCall API is settled.
type codexReplayCall struct {
	Identity [sha256.Size]byte
	Valid    bool
	CallID   string
	Status   string
}

func codexReplayID(item map[string]any) string {
	if item["type"] != "function_call" && item["type"] != "custom_tool_call" {
		return ""
	}
	id, _ := item["call_id"].(string)
	return id
}

func codexReplayIdentity(item map[string]any) ([sha256.Size]byte, string, bool) {
	var zero [sha256.Size]byte
	id := codexReplayID(item)
	name, ok := item["name"].(string)
	if id == "" || !ok || name == "" {
		return zero, id, false
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return zero, id, false
	}
	return sha256.Sum256(encoded), id, true
}

func codexReplaySnapshot(item map[string]any, status string) codexReplayCall {
	identity, id, ok := codexReplayIdentity(item)
	return codexReplayCall{Identity: identity, Valid: ok, CallID: id, Status: status}
}

// codexReplayMatch transfers status only when both the complete wire call and
// its correlation ID are unique on both sides. Unknown is represented by "".
func codexReplayMatch(original []codexReplayCall, output []any) []string {
	result := make([]string, len(output))
	oldIDs, newIDs := map[string]int{}, map[string]int{}
	oldIdentities, newIdentities := map[[sha256.Size]byte]int{}, map[[sha256.Size]byte]int{}
	byIdentity := map[[sha256.Size]byte]string{}
	for _, call := range original {
		if call.CallID != "" {
			oldIDs[call.CallID]++
		}
		if call.Valid {
			oldIdentities[call.Identity]++
			byIdentity[call.Identity] = call.Status
		}
	}
	identities := make([][sha256.Size]byte, len(output))
	valid := make([]bool, len(output))
	ids := make([]string, len(output))
	for i, entry := range output {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if id := codexReplayID(item); id != "" {
			newIDs[id]++
		}
		identity, id, ok := codexReplayIdentity(item)
		if !ok {
			continue
		}
		identities[i], ids[i], valid[i] = identity, id, true
		newIdentities[identity]++
	}
	for i, identity := range identities {
		if valid[i] && oldIDs[ids[i]] == 1 && newIDs[ids[i]] == 1 && oldIdentities[identity] == 1 && newIdentities[identity] == 1 {
			result[i] = byIdentity[identity]
		}
	}
	return result
}

// codexReplayIsolatedNames reserves declarations and every existing history
// name before allocation. declared must include all generated wire aliases as
// well as declared names, including aliases not yet present in input. The caller
// supplies positions of calls requiring
// isolation. Names contain no arguments, results, or raw untrusted identifiers.
func codexReplayIsolatedNames(input []any, declared map[string]bool, positions []int) (map[int]string, error) {
	occupied := make(map[string]bool, len(declared)+len(input))
	for name := range declared {
		occupied[name] = true
	}
	for _, entry := range input {
		if item, ok := entry.(map[string]any); ok {
			if name, ok := item["name"].(string); ok {
				occupied[name] = true
			}
		}
	}
	result := make(map[int]string, len(positions))
	for _, pos := range positions {
		if pos < 0 || pos >= len(input) {
			return nil, errors.New("Codex invalid isolation position")
		}
		if _, exists := result[pos]; exists {
			return nil, errors.New("Codex duplicate isolation position")
		}
		item, ok := input[pos].(map[string]any)
		if !ok {
			return nil, errors.New("Codex invalid isolation call")
		}
		_, id, ok := codexReplayIdentity(item)
		if !ok {
			return nil, errors.New("Codex invalid isolation call")
		}
		name := item["name"].(string)
		seed, err := json.Marshal([]any{id, name, pos})
		if err != nil {
			return nil, errors.New("Codex invalid isolation call")
		}
		sum := sha256.Sum256(seed)
		base := fmt.Sprintf("unknown_%x", sum[:16])
		found := false
		// Each failed candidate is occupied; one more than the occupied count
		// must succeed, unless the provider's name limit cannot represent it.
		for n := 0; n <= len(occupied); n++ {
			candidate := base
			if n != 0 {
				candidate = fmt.Sprintf("%s_%d", base, n)
			}
			if len(candidate) > 64 {
				break
			}
			if !occupied[candidate] {
				occupied[candidate], result[pos], found = true, candidate, true
				break
			}
		}
		if !found {
			return nil, errors.New("Codex isolated tool name space exhausted")
		}
	}
	return result, nil
}
