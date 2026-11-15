package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"
)

func TestRecoveryCallbackPanicAlwaysReleasesRun(t *testing.T) {
	for _, callback := range []string{"append", "listener"} {
		t.Run(callback, func(t *testing.T) {
			a := NewAgent(AgentOptions{})
			if callback == "append" {
				a.BeforeMessageEnd = func(context.Context, AgentMessage) error { panic("append panic") }
			} else {
				a.Subscribe(func(context.Context, AgentEvent) error { panic("listener panic") })
			}
			run, err := a.claimRun(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Error("expected callback panic")
					}
				}()
				_ = a.executeClaimedRun(run, func(context.Context) { panic("original") })
			}()
			select {
			case <-run.done:
			case <-time.After(time.Second):
				t.Fatal("run not released")
			}
			if run.ctx.Err() == nil || a.State().IsStreaming || a.active != nil {
				t.Fatal("run not cleaned up")
			}
			next, err := a.claimRun(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			a.releaseRun(next)
		})
	}
}

func TestRecoveryCommittedDiagnosticSurvivesListenerFailure(t *testing.T) {
	for _, fail := range []string{"error", "panic"} {
		t.Run(fail, func(t *testing.T) {
			a := NewAgent(AgentOptions{})
			var persisted []AgentMessage
			a.BeforeMessageEnd = func(_ context.Context, m AgentMessage) error { persisted = append(persisted, m); return nil }
			a.Subscribe(func(_ context.Context, e AgentEvent) error {
				if e.Type == EvMessageStart {
					if fail == "panic" {
						panic("listener")
					}
					return errors.New("listener")
				}
				return nil
			})
			run, _ := a.claimRun(context.Background(), nil)
			func() {
				defer func() { _ = recover() }()
				_ = a.executeClaimedRun(run, func(context.Context) { panic("original") })
			}()
			if len(persisted) != 1 || len(a.State().Messages) != 1 || a.State().Messages[0] != persisted[0] {
				t.Fatalf("persisted=%v state=%v", persisted, a.State().Messages)
			}
		})
	}
}

func TestRecoveryTerminalMessagesAndAppendError(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "rejected"}[reject], func(t *testing.T) {
			a := NewAgent(AgentOptions{})
			prior := &ai.UserMessage{}
			current := &ai.UserMessage{}
			a.state.Messages = []AgentMessage{prior}
			if reject {
				a.BeforeMessageEnd = func(_ context.Context, m AgentMessage) error {
					if _, ok := m.(*ai.AssistantMessage); ok {
						return errors.New("append failed")
					}
					return nil
				}
			}
			var terminal AgentEvent
			var failure *ai.AssistantMessage
			a.Subscribe(func(_ context.Context, e AgentEvent) error {
				if e.Type == EvTurnEnd {
					failure, _ = e.Message.(*ai.AssistantMessage)
				}
				if e.Type == EvAgentEnd {
					terminal = e
				}
				return nil
			})
			run, _ := a.claimRun(context.Background(), nil)
			err := a.executeClaimedRun(run, func(ctx context.Context) {
				if err := a.processEvent(ctx)(AgentEvent{Type: EvMessageEnd, Message: current}); err != nil {
					panic(err)
				}
				panic("original failure")
			})
			// Only the synthetic diagnostic append is rejected.
			_ = err
			if failure == nil || !strings.Contains(failure.ErrorMessage, "original failure") {
				t.Fatalf("failure=%+v", failure)
			}
			if reject && !strings.Contains(failure.ErrorMessage, "append failed") {
				t.Fatalf("failure=%+v", failure)
			}
			want := 2
			if reject {
				want = 1
			}
			if len(terminal.Messages) != want || terminal.Messages[0] != current {
				t.Fatalf("terminal=%v", terminal.Messages)
			}
		})
	}
}
