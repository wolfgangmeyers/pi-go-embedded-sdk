package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-valley/pi/ai"
)

func TestRestoredCheckpointAppliesWithoutAutomaticCompaction(t *testing.T) {
	s := NewSession(SessionOptions{SessionID: "checkpoint"})
	msgs := []OrderedMessage{{Sequence: 1, ParentSequence: 0, Message: ai.NewUserText("discarded", 1)}, {Sequence: 2, ParentSequence: 1, Message: ai.NewUserText("retained", 2)}}
	if err := s.RestoreOrdered(context.Background(), OrderedSnapshot{SessionID: "checkpoint", Messages: msgs, Checkpoint: &OrderedCheckpoint{ThroughSequence: 2, FirstKeptSequence: 2, Summary: "saved summary"}}); err != nil {
		t.Fatal(err)
	}
	view := s.Agent.TransformContext(context.Background(), s.History())
	var text string
	for _, m := range view {
		if u, ok := m.(ai.UserMessage); ok {
			for _, c := range u.Content {
				if v, ok := c.(ai.TextContent); ok {
					text += v.Text
				}
			}
		}
	}
	if strings.Contains(text, "discarded") || !strings.Contains(text, "retained") || !strings.Contains(text, "saved summary") {
		t.Fatalf("unexpected view %q (%v)", text, view)
	}
	if s.compactTransform != nil {
		t.Fatal("automatic compaction enabled")
	}
}
