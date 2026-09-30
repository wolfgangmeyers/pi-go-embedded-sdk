package coding

import (
	"context"
	"fmt"
	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
)

// OrderedPersistence is a host-owned durable transaction boundary. An error
// means the run cannot proceed; the host owns retries, fencing and storage.
type OrderedPersistence interface {
	AppendMessage(context.Context, agent.AgentMessage) error
	CommitCheckpoint(context.Context, OrderedCheckpoint) error
}

type OrderedMessage struct {
	Sequence       int64
	ParentSequence int64
	Message        agent.AgentMessage
}
type OrderedCheckpoint struct {
	ThroughSequence   int64
	FirstKeptSequence int64
	Summary           string
}
type OrderedSnapshot struct {
	SessionID  string
	Messages   []OrderedMessage
	Checkpoint *OrderedCheckpoint
}

// RestoreOrdered validates the entire committed chain before publishing any
// context. It never executes tools or retries an uncertain effect.
func (s *Session) RestoreOrdered(ctx context.Context, snap OrderedSnapshot) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if snap.SessionID == "" || snap.SessionID != s.sessionID {
		return fmt.Errorf("ordered session binding mismatch")
	}
	messages := make([]agent.AgentMessage, 0, len(snap.Messages))
	for i, entry := range snap.Messages {
		if entry.Sequence != int64(i+1) || entry.ParentSequence != int64(i) || entry.Message == nil {
			return fmt.Errorf("invalid ordered ancestry at %d", i+1)
		}
		messages = append(messages, entry.Message)
	}
	var checkpoint *compactionCheckpoint
	if c := snap.Checkpoint; c != nil {
		if c.ThroughSequence > int64(len(messages)) || c.ThroughSequence <= 0 || c.FirstKeptSequence < 1 || c.FirstKeptSequence > c.ThroughSequence || c.Summary == "" {
			return fmt.Errorf("invalid checkpoint ancestry")
		}
		checkpoint = &compactionCheckpoint{prefixLen: int(c.FirstKeptSequence - 1), compactedLen: int(c.ThroughSequence), summary: c.Summary}
		if system, ok := ai.GetCurrentSystemMessage(messages); ok {
			checkpoint.systemMessage = &system
		}
	}
	if s.Agent.State().IsStreaming {
		return fmt.Errorf("cannot restore during run")
	}
	s.Agent.SetMessages(messages)
	s.setCompaction(checkpoint, len(messages))
	return nil
}
