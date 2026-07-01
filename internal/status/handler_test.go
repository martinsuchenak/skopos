package status

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testHandler(t *testing.T, apiKey string) *Handler {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewHandler(NewService(NewStorage(sqlDB)), apiKey)
}

func TestHandlerReportRequiresAPIKey(t *testing.T) {
	handler := testHandler(t, "secret")
	req := httptest.NewRequest("POST", "/api/reports", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()

	handler.Report(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestHandlerReportCreatesReport(t *testing.T) {
	handler := testHandler(t, "secret")
	body := bytes.NewBufferString(`{
		"agent_id":"codex-1",
		"agent_type":"codex",
		"workspace":"/repo",
		"status":"running",
		"message":"working"
	}`)
	req := httptest.NewRequest("POST", "/api/reports", body)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()

	handler.Report(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d body=%s", http.StatusCreated, w.Code, w.Body.String())
	}
	var result ReportResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.SessionID == "" || result.EventID == "" {
		t.Fatalf("expected ids, got %#v", result)
	}

	sessions, err := handler.service.ListSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
}

func TestHandlerReportRejectsInvalidPayload(t *testing.T) {
	handler := testHandler(t, "")
	req := httptest.NewRequest("POST", "/api/reports", bytes.NewBufferString(`{"status":"running"}`))
	w := httptest.NewRecorder()

	handler.Report(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandlerDeleteSessionRequiresAPIKey(t *testing.T) {
	handler := testHandler(t, "secret")
	req := httptest.NewRequest("DELETE", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	w := httptest.NewRecorder()

	handler.DeleteSession(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestHandlerListSessionsFiltersByWorkspace(t *testing.T) {
	handler := testHandler(t, "secret")

	for _, ws := range []string{"/repo-a", "/repo-a", "/repo-b"} {
		body := bytes.NewBufferString(`{
			"agent_id":"agent-1",
			"agent_type":"codex",
			"workspace":"` + ws + `",
			"status":"running"
		}`)
		req := httptest.NewRequest("POST", "/api/reports", body)
		req.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		handler.Report(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("create report: expected %d, got %d", http.StatusCreated, w.Code)
		}
	}

	req := httptest.NewRequest("GET", "/api/sessions?workspace=/repo-a", nil)
	w := httptest.NewRecorder()
	handler.ListSessions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, w.Code)
	}
	var sessions []SessionSummary
	if err := json.NewDecoder(w.Body).Decode(&sessions); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions for /repo-a, got %d", len(sessions))
	}
	if sessions[0].Workspace != "/repo-a" {
		t.Fatalf("expected workspace /repo-a, got %q", sessions[0].Workspace)
	}

	req = httptest.NewRequest("GET", "/api/sessions", nil)
	w = httptest.NewRecorder()
	handler.ListSessions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, w.Code)
	}
	var all []SessionSummary
	if err := json.NewDecoder(w.Body).Decode(&all); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 sessions total, got %d", len(all))
	}
}
