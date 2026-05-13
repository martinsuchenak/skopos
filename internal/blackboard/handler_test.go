package blackboard

import (
	"bytes"
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
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable fk: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewHandler(NewService(NewStorage(sqlDB)), apiKey)
}

func TestHandlerWriteRequiresAPIKey(t *testing.T) {
	h := testHandler(t, "secret")
	body := bytes.NewBufferString(`{
		"scope":"project","entry_type":"finding","title":"T","author_agent_id":"a"
	}`)
	req := httptest.NewRequest("POST", "/api/blackboard/entries", body)
	w := httptest.NewRecorder()
	h.WriteEntry(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandlerWriteAndReadBundle(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{
		"scope":"project","entry_type":"finding","title":"Auth issue",
		"content":"Details.","author_agent_id":"agent-1"
	}`)
	req := httptest.NewRequest("POST", "/api/blackboard/entries", body)
	w := httptest.NewRecorder()
	h.WriteEntry(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", w.Code, w.Body.String())
	}
	var result WriteResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode write result: %v", err)
	}
	if result.ID == "" {
		t.Fatal("expected ID in result")
	}

	req2 := httptest.NewRequest("GET", "/api/blackboard/entries", nil)
	w2 := httptest.NewRecorder()
	h.ReadBundle(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	var bundle Bundle
	if err := json.NewDecoder(w2.Body).Decode(&bundle); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if len(bundle.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(bundle.Entries))
	}
	if bundle.MarkdownBundle == "" {
		t.Fatal("expected non-empty MarkdownBundle")
	}
}

func TestHandlerWriteRejectsInvalidPayload(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{"scope":"project","entry_type":"finding"}`)
	req := httptest.NewRequest("POST", "/api/blackboard/entries", body)
	w := httptest.NewRecorder()
	h.WriteEntry(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandlerWriteRejectsInvalidScope(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{
		"scope":"global","entry_type":"finding","title":"T","author_agent_id":"a"
	}`)
	req := httptest.NewRequest("POST", "/api/blackboard/entries", body)
	w := httptest.NewRecorder()
	h.WriteEntry(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandlerPromoteNotFound(t *testing.T) {
	h := testHandler(t, "")
	req := httptest.NewRequest("PATCH", "/api/blackboard/entries/missing/promote", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	h.Promote(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandlerDeleteNotFound(t *testing.T) {
	h := testHandler(t, "")
	req := httptest.NewRequest("DELETE", "/api/blackboard/entries/missing", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	h.Delete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
