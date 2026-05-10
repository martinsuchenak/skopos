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
	req.Header.Set("X-API-Key", "secret")
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

	sessions, err := handler.service.ListSessions(context.Background())
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
