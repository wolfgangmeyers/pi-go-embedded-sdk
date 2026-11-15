package ai

import "time"

// nowMillis returns the current Unix time in milliseconds.
func nowMillis() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}

// Clone returns a deep copy of an AssistantMessage, used when emitting partial
// snapshots so downstream consumers don't observe later mutations.
func (m *AssistantMessage) Clone() *AssistantMessage {
	if m == nil {
		return nil
	}
	cp := *m
	cp.Content = append(ContentList(nil), m.Content...)
	for i, content := range cp.Content {
		if call, ok := content.(ToolCall); ok && call.CodexReplayProvenance != nil {
			provenance := *call.CodexReplayProvenance
			call.CodexReplayProvenance = &provenance
			cp.Content[i] = call
		}
	}
	if m.Diagnostics != nil {
		cp.Diagnostics = append([]Diagnostic(nil), m.Diagnostics...)
	}
	if m.Deferred != nil {
		handle := *m.Deferred // Data stays shared: it is opaque provider JSON.
		cp.Deferred = &handle
	}
	return &cp
}
