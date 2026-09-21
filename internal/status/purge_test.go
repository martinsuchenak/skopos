package status

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/events"
)

type capturePublisher struct{ got []events.Event }

func (c *capturePublisher) Publish(e events.Event) { c.got = append(c.got, e) }

// purgeStorage mirrors the production DSN (foreign_keys ON): the package's
// shared testStorage uses a bare :memory: DSN where FK cascades silently
// no-op, and the purge cascade is exactly what these tests assert.
func purgeStorage(t *testing.T) *Storage {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "purge.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return NewStorage(sqlDB)
}

func seedSession(t *testing.T, svc *Service, ws, sessionID string) {
	t.Helper()
	if _, err := svc.Report(context.Background(), ReportInput{
		AgentID: "agent-1", AgentType: "zcode", Workspace: ws, Status: StatusRunning,
		SessionID: sessionID, Message: "seed",
	}); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, s *Storage, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStoragePurgeWorkspaceCascades(t *testing.T) {
	s := purgeStorage(t)
	svc := NewService(s)
	seedSession(t, svc, "ws-a", "sess-a1")
	seedSession(t, svc, "ws-a", "sess-a2")
	seedSession(t, svc, "ws-b", "sess-b1")
	// A session-scoped blackboard entry rides the FK cascade.
	if _, err := s.db.Exec(`INSERT INTO blackboard_entries (id, scope, workspace_id, session_id, entry_type, title, content, author_agent_id, created_at, updated_at)
		VALUES ('e1', 'session', 'ws-a', 'sess-a1', 'context', 'note', 'x', 'agent-1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteAllSessions(context.Background(), "ws-a")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 sessions purged, got %d", n)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM sessions WHERE workspace = 'ws-a'`); got != 0 {
		t.Fatalf("ws-a sessions survived: %d", got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM events WHERE session_id IN ('sess-a1','sess-a2')`); got != 0 {
		t.Fatalf("ws-a events survived: %d", got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM agent_states WHERE session_id IN ('sess-a1','sess-a2')`); got != 0 {
		t.Fatalf("ws-a agent states survived: %d", got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM blackboard_entries WHERE id = 'e1'`); got != 0 {
		t.Fatalf("session-scoped blackboard entry survived the cascade: %d", got)
	}
	// The other workspace is untouched.
	if got := countRows(t, s, `SELECT COUNT(*) FROM sessions WHERE workspace = 'ws-b'`); got != 1 {
		t.Fatalf("ws-b sessions altered: %d", got)
	}

	n, err = s.DeleteAllSessions(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 session in the purge-all, got %d", n)
	}
}

func TestServicePurgeScopeMatrix(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store)

	// No principal = root/internal caller: purge-everything is allowed.
	n, err := svc.PurgeSessions(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || store.deletedAll != 1 {
		t.Fatalf("expected root purge to hit the store once, got n=%d calls=%d", n, store.deletedAll)
	}

	// Scoped key, own workspace: allowed.
	if _, err := svc.PurgeSessions(scopedCtx(), " ws-a "); err != nil {
		t.Fatalf("scoped purge of own workspace failed: %v", err)
	}
	// Scoped key, foreign workspace: actionable out-of-scope.
	if _, err := svc.PurgeSessions(scopedCtx(), "ws-b"); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("expected ErrOutOfScope for foreign workspace, got %v", err)
	}
	// Scoped key, no workspace: purge-everything is root-only.
	if _, err := svc.PurgeSessions(scopedCtx(), ""); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("expected ErrRootRequired for scoped purge-all, got %v", err)
	}
}

func TestServicePurgePublishesSessionsAndBlackboardEvents(t *testing.T) {
	s := purgeStorage(t)
	svc := NewService(s)
	seedSession(t, svc, "ws-a", "sess-a1")
	pub := &capturePublisher{}
	svc.SetPublisher(pub)

	if _, err := svc.PurgeSessions(context.Background(), "ws-a"); err != nil {
		t.Fatal(err)
	}
	if len(pub.got) != 2 {
		t.Fatalf("expected sessions+blackboard events, got %d: %+v", len(pub.got), pub.got)
	}
	for _, e := range pub.got {
		if e.Workspace != "ws-a" {
			t.Fatalf("expected attributed workspace ws-a, got %+v", e)
		}
	}
	if pub.got[0].Type != events.TypeSessions || pub.got[1].Type != events.TypeBlackboard {
		t.Fatalf("unexpected event types: %+v", pub.got)
	}
}

func TestHandlerPurgeSessions(t *testing.T) {
	h := NewHandler(NewService(testStorage(t)), testAuth("k"))

	r := httptest.NewRequest(http.MethodDelete, "/api/sessions", nil)
	r.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.PurgeSessions(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Deleted int `json:"deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	// A scoped key attempting the all-workspaces purge gets a 403.
	r = httptest.NewRequest(http.MethodDelete, "/api/sessions", nil)
	r.Header.Set("Authorization", "Bearer k")
	r = r.WithContext(auth.WithPrincipal(r.Context(), &auth.Principal{KeyID: "k1", Workspaces: map[string]struct{}{"ws-a": {}}}))
	w = httptest.NewRecorder()
	h.PurgeSessions(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for scoped purge-all, got %d", w.Code)
	}
}
