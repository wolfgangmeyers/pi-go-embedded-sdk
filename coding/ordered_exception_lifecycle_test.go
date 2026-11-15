package coding_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"
)

func TestOrderedExceptionalFailureCommitsBeforePublishing(t *testing.T) {
	for _, afterStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-stream-start", true: "after-stream-start"}[afterStart], func(t *testing.T) {
			h := openConversation(t, filepath.Join(t.TempDir(), "store.sqlite"), "exception")
			s, done := fixture(t, h, answer("answer"))
			defer done()
			if !afterStart {
				s.Agent.StreamFn = func(context.Context, *ai.Model, ai.TranscriptContext, *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
					panic("provider exploded")
				}
			}
			rec, err := codingRecorder(t, s)
			if err != nil {
				t.Fatal(err)
			}
			defer rec.Close()
			var sawDiagnostic, terminal bool
			failed := false
			s.Agent.Subscribe(func(_ context.Context, e agent.AgentEvent) error {
				if afterStart && !failed && e.Type == agent.EvMessageStart && e.Message.MessageRole() == ai.RoleAssistant {
					failed = true
					return errors.New("listener exploded")
				}
				if e.Type == agent.EvMessageEnd {
					if m, ok := e.Message.(*ai.AssistantMessage); ok && strings.Contains(m.ErrorMessage, "exploded") {
						var n int
						if err := h.db.QueryRow(`SELECT COUNT(*) FROM entries WHERE session=? AND role='assistant'`, h.session).Scan(&n); err != nil || n != 1 {
							t.Fatalf("diagnostic published before commit: count=%d err=%v", n, err)
						}
						sawDiagnostic = true
					}
				}
				if e.Type == agent.EvAgentEnd {
					terminal = true
					if len(e.Messages) != 3 || e.Messages[2].MessageRole() != ai.RoleAssistant {
						t.Fatalf("terminal messages: %v", e.Messages)
					}
				}
				return nil
			})
			result, err := s.Run(context.Background(), "go")
			if err == nil || result == nil || result.StopReason != ai.StopError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !sawDiagnostic || !terminal || s.Agent.State().IsStreaming || s.Agent.State().StreamingMessage != nil {
				t.Fatalf("diagnostic=%v terminal=%v state=%+v", sawDiagnostic, terminal, s.Agent.State())
			}
			data, err := os.ReadFile(rec.Path())
			if err != nil || !strings.Contains(string(data), "exploded") {
				t.Fatalf("recorder=%s err=%v", data, err)
			}
		})
	}
}

func codingRecorder(t *testing.T, s *coding.Session) (*coding.SessionRecorder, error) {
	t.Helper()
	rec, err := coding.StartSession(t.TempDir(), s.Model)
	if err == nil {
		s.Record(rec)
	}
	return rec, err
}

func TestOrderedRejectionAfterStreamStartIsTerminalOnly(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "store.sqlite"), "rejected")
	s, done := fixture(t, h, answer("not committed"))
	defer done()
	rec, err := codingRecorder(t, s)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	var started, diagnostic, terminal bool
	s.Agent.Subscribe(func(_ context.Context, e agent.AgentEvent) error {
		switch e.Type {
		case agent.EvMessageStart:
			if e.Message.MessageRole() == ai.RoleAssistant {
				started = true
				h.failAppend = true
			}
		case agent.EvMessageEnd:
			if e.Message.MessageRole() == ai.RoleAssistant {
				t.Error("published rejected assistant")
			}
		case agent.EvTurnEnd:
			if m, ok := e.Message.(*ai.AssistantMessage); ok && strings.Contains(m.ErrorMessage, "injected append") {
				diagnostic = true
			}
		case agent.EvAgentEnd:
			terminal = true
			if len(e.Messages) != 2 || e.Messages[1].MessageRole() != ai.RoleUser {
				t.Errorf("uncommitted terminal messages: %v", e.Messages)
			}
		}
		return nil
	})
	result, err := s.Run(context.Background(), "go")
	if err == nil || result == nil || result.StopReason != ai.StopError || !started || !diagnostic || !terminal {
		t.Fatalf("result=%+v err=%v start=%v diagnostic=%v terminal=%v", result, err, started, diagnostic, terminal)
	}
	st := s.Agent.State()
	if st.IsStreaming || st.StreamingMessage != nil || !strings.Contains(st.ErrorMessage, "injected append") {
		t.Fatalf("state=%+v", st)
	}
	data, err := os.ReadFile(rec.Path())
	if err != nil || strings.Contains(string(data), "not committed") || strings.Contains(string(data), "injected append") {
		t.Fatalf("recorder=%s err=%v", data, err)
	}
}

func TestOrderedAbortedRejectionHasNoCommittedTerminalMessage(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "store.sqlite"), "aborted")
	s, done := fixture(t, h, answer("not committed"))
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var terminal, aborted bool
	s.Agent.Subscribe(func(_ context.Context, e agent.AgentEvent) error {
		switch e.Type {
		case agent.EvMessageStart:
			if e.Message.MessageRole() == ai.RoleAssistant {
				h.failAppend = true
				cancel()
			}
		case agent.EvTurnEnd:
			if m, ok := e.Message.(*ai.AssistantMessage); ok {
				aborted = m.StopReason == ai.StopAborted
			}
		case agent.EvAgentEnd:
			terminal = true
			if len(e.Messages) != 2 || e.Messages[1].MessageRole() != ai.RoleUser {
				t.Errorf("uncommitted terminal messages: %v", e.Messages)
			}
		}
		return nil
	})
	_, err := s.Run(ctx, "go")
	if err == nil || !aborted || !terminal {
		t.Fatalf("err=%v aborted=%v terminal=%v", err, aborted, terminal)
	}
	st := s.Agent.State()
	if st.IsStreaming || st.StreamingMessage != nil {
		t.Fatalf("state=%+v", st)
	}
}
