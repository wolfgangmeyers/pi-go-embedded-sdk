package coding_test

// RED contract for a host-owned SQLite conversation. This file deliberately
// references the proposed SDK API, which does not exist at the pinned revision.
// It is not a production SQLite dependency or router schema.
import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/coding"
	_ "modernc.org/sqlite"
)

// Proposed minimal public API: SessionOptions.OrderedPersistence accepts a
// checked host callback with AppendMessage(ctx, message) error and
// CommitCheckpoint(ctx, checkpoint) error. RestoreOrdered(ctx, snapshot) error
// validates ordered parent/sequence and checkpoint ancestry before publication.
// Compact(ctx) (bool,error) is an explicit safe-boundary compaction request;
// false means no checkpoint. SDK doesn't own SQLite or decide mail ACK/retry.
// Snapshot carries only committed records; host supplies stable session binding.
type sqliteConversation struct {
	db             *sql.DB
	session        string
	failAppend     bool
	failCheckpoint bool
}

func openConversation(t *testing.T, path, session string) *sqliteConversation {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS entries (session TEXT NOT NULL, seq INTEGER NOT NULL, parent INTEGER NOT NULL, role TEXT NOT NULL, body BLOB NOT NULL, digest BLOB NOT NULL, PRIMARY KEY(session,seq))`,
		`CREATE TABLE IF NOT EXISTS checkpoints (session TEXT PRIMARY KEY, through_seq INTEGER NOT NULL, first_kept_seq INTEGER NOT NULL, summary TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	return &sqliteConversation{db: db, session: session}
}

func (h *sqliteConversation) AppendMessage(ctx context.Context, m agent.AgentMessage) error {
	if h.failAppend {
		return errors.New("injected append transaction failure")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var next, parent int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM entries WHERE session=?`, h.session).Scan(&parent); err != nil {
		return err
	}
	next = parent + 1
	digest := sha256.Sum256(body)
	if _, err := tx.ExecContext(ctx, `INSERT INTO entries(session,seq,parent,role,body,digest) VALUES(?,?,?,?,?,?)`, h.session, next, parent, string(m.MessageRole()), body, digest[:]); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *sqliteConversation) CommitCheckpoint(ctx context.Context, c coding.OrderedCheckpoint) error {
	if h.failCheckpoint {
		return errors.New("injected checkpoint commit failure")
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tip int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM entries WHERE session=?`, h.session).Scan(&tip); err != nil {
		return err
	}
	if c.ThroughSequence != tip || c.FirstKeptSequence <= 0 || c.FirstKeptSequence > tip {
		return fmt.Errorf("checkpoint ancestry: %+v at tip %d", c, tip)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints(session,through_seq,first_kept_seq,summary) VALUES(?,?,?,?) ON CONFLICT(session) DO UPDATE SET through_seq=excluded.through_seq,first_kept_seq=excluded.first_kept_seq,summary=excluded.summary`, h.session, c.ThroughSequence, c.FirstKeptSequence, c.Summary); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *sqliteConversation) snapshot(ctx context.Context) (coding.OrderedSnapshot, error) {
	result := coding.OrderedSnapshot{SessionID: h.session}
	rows, err := h.db.QueryContext(ctx, `SELECT seq,parent,role,body,digest FROM entries WHERE session=? ORDER BY seq`, h.session)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var seq, parent int64
		var role string
		var body, digest []byte
		if err = rows.Scan(&seq, &parent, &role, &body, &digest); err != nil {
			break
		}
		want := sha256.Sum256(body)
		if seq != int64(len(result.Messages))+1 || parent != seq-1 || string(want[:]) != string(digest) {
			err = fmt.Errorf("corrupt ordered entry %d", seq)
			break
		}
		m, decodeErr := ai.UnmarshalMessage(body)
		if decodeErr != nil || string(m.MessageRole()) != role {
			err = fmt.Errorf("corrupt message %d: %v", seq, decodeErr)
			break
		}
		result.Messages = append(result.Messages, coding.OrderedMessage{Sequence: seq, ParentSequence: parent, Message: m})
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return result, err
	}
	var c coding.OrderedCheckpoint
	err = h.db.QueryRowContext(ctx, `SELECT through_seq,first_kept_seq,summary FROM checkpoints WHERE session=?`, h.session).Scan(&c.ThroughSequence, &c.FirstKeptSequence, &c.Summary)
	if err != nil && err != sql.ErrNoRows {
		return result, err
	}
	if err == nil {
		result.Checkpoint = &c
	}
	return result, nil
}

func roles(t *testing.T, h *sqliteConversation) []string {
	t.Helper()
	rows, err := h.db.Query(`SELECT role FROM entries WHERE session=? ORDER BY seq`, h.session)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			t.Fatal(err)
		}
		got = append(got, role)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func fixture(t *testing.T, h *sqliteConversation, steps ...providers.FauxResponseStep) (*coding.Session, func()) {
	t.Helper()
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	reg.SetResponses(steps)
	s := coding.NewSession(coding.SessionOptions{SessionID: h.session, Model: reg.GetModel(), Cwd: t.TempDir(), NoTools: coding.NoToolsAll, OrderedPersistence: h})
	return s, reg.Unregister
}

func answer(text string) providers.FauxResponseStep {
	return providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: text}}, ai.StopStop))
}

func TestEmbeddedOrderedFirstPromptCommittedBeforeProvider(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "conversation.sqlite"), "root-epoch-1")
	s, done := fixture(t, h, func(_ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		got := roles(t, h)
		if len(got) != 2 || got[0] != "system" || got[1] != "user" {
			t.Errorf("provider saw uncommitted first prompt: %v", got)
		}
		return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "done"}}, ai.StopStop)
	})
	defer done()
	if _, err := s.Run(context.Background(), "first prompt"); err != nil {
		t.Fatal(err)
	}
	if got := roles(t, h); strings.Join(got, ",") != "system,user,assistant" {
		t.Fatalf("persisted roles %v", got)
	}
}

func TestEmbeddedOrderedToolResultCommittedBeforeContinuationAndReopen(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "conversation.sqlite"), "worker-epoch-1")
	reg := providers.RegisterFauxProvider(providers.RegisterFauxProviderOptions{})
	defer reg.Unregister()
	count := 0
	reg.SetResponses([]providers.FauxResponseStep{
		providers.FauxStatic(providers.FauxAssistantMessage(ai.ContentList{providers.FauxToolCall("read", map[string]any{}, "call-1")}, ai.StopToolUse)),
		func(_ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			if got := strings.Join(roles(t, h), ","); got != "system,user,assistant,toolResult" {
				t.Errorf("continuation before checked tool append: %s", got)
			}
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "complete"}}, ai.StopStop)
		},
	})
	tool := agent.AgentTool{Name: "read", Description: "read fixture", Parameters: &ai.Schema{Type: "object"}, Execute: func(_ context.Context, _ string, _ map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
		count++
		return agent.AgentToolResult{Content: ai.ContentList{ai.TextContent{Text: "fixture result"}}}, nil
	}}
	s := coding.NewSession(coding.SessionOptions{SessionID: h.session, Model: reg.GetModel(), Cwd: t.TempDir(), Tools: []agent.AgentTool{tool}, OrderedPersistence: h})
	if _, err := s.Run(context.Background(), "run one tool"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reopened := coding.NewSession(coding.SessionOptions{SessionID: h.session, Model: reg.GetModel(), Cwd: t.TempDir(), Tools: []agent.AgentTool{tool}, OrderedPersistence: h})
	if err := reopened.RestoreOrdered(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(reopened.History()) != 5 || count != 1 {
		t.Fatalf("reopen lost ordered conversation or replayed effect: len=%d calls=%d", len(reopened.History()), count)
	}
	snapshot.Messages[2].ParentSequence = 0
	if err := reopened.RestoreOrdered(context.Background(), snapshot); err == nil {
		t.Fatal("broken ancestry accepted")
	}
}

func TestEmbeddedOrderedWriteFailureStopsBeforeProviderAndDoesNotClaimSuccess(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "conversation.sqlite"), "failed-epoch")
	h.failAppend = true
	called := false
	s, done := fixture(t, h, func(_ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
		called = true
		return providers.FauxAssistantMessage(nil, ai.StopStop)
	})
	defer done()
	if _, err := s.Run(context.Background(), "must persist"); err == nil || !strings.Contains(err.Error(), "injected append") {
		t.Fatalf("unchecked append failure: %v", err)
	}
	if called || len(roles(t, h)) != 0 {
		t.Fatal("provider ran after failed first commit")
	}
	h.failAppend = false
	if _, err := h.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), "must persist"); err == nil {
		t.Fatal("SQLite write failure ignored")
	}
	if called {
		t.Fatal("provider ran after SQLite write failure")
	}
}

func TestEmbeddedOrderedCreateFailureSurfaces(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent", "uncreatable.sqlite")
	db, err := sql.Open("sqlite", missing)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := &sqliteConversation{db: db, session: "create-failure"}
	s, done := fixture(t, h, answer("must not be sent"))
	defer done()
	if _, err := s.Run(context.Background(), "first prompt"); err == nil {
		t.Fatal("database create failure swallowed")
	}
}

func TestEmbeddedOrderedAutomaticCheckpointFailureStopsBeforeProvider(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "auto.sqlite"), "auto-epoch")
	calls := 0
	s, done := fixture(t, h,
		func(_ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			calls++
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "first"}}, ai.StopStop)
		},
		answer("summary"),
		func(_ ai.TranscriptContext, _ *ai.SimpleStreamOptions, _ *providers.FauxState, _ *ai.Model) *ai.AssistantMessage {
			calls++
			return providers.FauxAssistantMessage(ai.ContentList{ai.TextContent{Text: "unsafe"}}, ai.StopStop)
		},
	)
	defer done()
	if _, err := s.Run(context.Background(), strings.Repeat("prior ", 400)); err != nil {
		t.Fatal(err)
	}
	s.Model.ContextWindow = 10
	s.EnableCompaction(coding.CompactionSettings{Enabled: true, ReserveTokens: 1, KeepRecentTokens: 1})
	h.failCheckpoint = true
	before := len(s.History())
	if _, err := s.Run(context.Background(), "trigger"); err == nil || !strings.Contains(err.Error(), "checkpoint commit") {
		t.Fatalf("automatic checkpoint failure: %v", err)
	}
	if calls != 1 {
		t.Fatalf("provider continuation ran: %d", calls)
	}
	snap, err := h.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Checkpoint != nil || len(s.History()) < before {
		t.Fatal("published failed checkpoint")
	}
}

func TestEmbeddedOrderedSuccessfulCheckpointAndUnknownEffectReopen(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "success.sqlite"), "effect-epoch")
	s, done := fixture(t, h, answer("one"), answer("two"), answer("summary"), answer("split-summary"))
	defer done()
	if _, err := s.Run(context.Background(), "older"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), "newer"); err != nil {
		t.Fatal(err)
	}
	s.EnableCompaction(coding.CompactionSettings{Enabled: true, ReserveTokens: 1, KeepRecentTokens: 1})
	ok, err := s.Compact(context.Background())
	if err != nil || !ok {
		t.Fatalf("checkpoint: %v %v", ok, err)
	}
	snap, err := h.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Checkpoint == nil || snap.Checkpoint.ThroughSequence != int64(len(snap.Messages)) {
		t.Fatal("missing committed checkpoint")
	}
	reopened, cleanup := fixture(t, h)
	defer cleanup()
	if err := reopened.RestoreOrdered(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if len(reopened.History()) != len(s.History()) {
		t.Fatal("restore lost ordered history")
	}
	// An executed but uncommitted tool result is not in the snapshot. Restore
	// must not dispatch or synthesize the missing effect; router fences it later.
	snap.Messages = snap.Messages[:len(snap.Messages)-1]
	snap.Checkpoint = nil
	if err := reopened.RestoreOrdered(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if len(reopened.History()) != len(snap.Messages) {
		t.Fatal("unknown effect was replayed")
	}
}

func TestEmbeddedOrderedCheckpointCommitFailureRetainsOldContext(t *testing.T) {
	h := openConversation(t, filepath.Join(t.TempDir(), "conversation.sqlite"), "compact-epoch")
	s, done := fixture(t, h, answer("first"), answer("second"), answer("summary-1"), answer("summary-2"), answer("third"), answer("fourth"))
	defer done()
	if _, err := s.Run(context.Background(), strings.Repeat("long prior context ", 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), "another turn"); err != nil {
		t.Fatal(err)
	}
	s.EnableCompaction(coding.CompactionSettings{Enabled: true, ReserveTokens: 1, KeepRecentTokens: 1})
	h.failCheckpoint = true
	before := s.History()
	ok, err := s.Compact(context.Background())
	if err == nil || ok {
		t.Fatalf("failed checkpoint published: ok=%v err=%v", ok, err)
	}
	snap, err := h.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Checkpoint != nil || len(s.History()) != len(before) {
		t.Fatal("failed checkpoint displaced old context")
	}
	h.failCheckpoint = false
	s.EnableCompaction(coding.CompactionSettings{Enabled: false, KeepRecentTokens: 1})
	// A fresh turn and reopen must still see the old prompt, not a summary.
	if _, err := s.Run(context.Background(), "after failed compact"); err != nil {
		t.Fatal(err)
	}
	reopened, cleanup := fixture(t, h, answer("resumed"))
	defer cleanup()
	snap, err = h.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.RestoreOrdered(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if len(reopened.History()) != len(s.History()) {
		t.Fatal("failed checkpoint changed restored context")
	}
	// Corrupt checkpoint ancestry must be rejected before publishing any state.
	snap.Checkpoint = &coding.OrderedCheckpoint{ThroughSequence: 999, FirstKeptSequence: 1, Summary: "forged"}
	if err := reopened.RestoreOrdered(context.Background(), snap); err == nil {
		t.Fatal("forged checkpoint accepted")
	}
}
