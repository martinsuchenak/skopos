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

func TestHandlerPromoteAlreadyAtTopScope(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{
		"scope":"project","entry_type":"finding","title":"T","author_agent_id":"a"
	}`)
	req := httptest.NewRequest("POST", "/api/blackboard/entries", body)
	w := httptest.NewRecorder()
	h.WriteEntry(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("setup write: %d %s", w.Code, w.Body)
	}
	var result WriteResult
	json.NewDecoder(w.Body).Decode(&result)

	req2 := httptest.NewRequest("PATCH", "/api/blackboard/entries/"+result.ID+"/promote", nil)
	req2.SetPathValue("id", result.ID)
	w2 := httptest.NewRecorder()
	h.Promote(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d %s", w2.Code, w2.Body)
	}
}

func TestHandlerReadBundleWorkspaceFilter(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{
		"scope":"project","entry_type":"finding","title":"Scoped entry",
		"author_agent_id":"a","workspace_id":"ws-1"
	}`)
	req := httptest.NewRequest("POST", "/api/blackboard/entries", body)
	w := httptest.NewRecorder()
	h.WriteEntry(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("write: %d %s", w.Code, w.Body)
	}

	body2 := bytes.NewBufferString(`{
		"scope":"project","entry_type":"finding","title":"Global entry",
		"author_agent_id":"a"
	}`)
	req2 := httptest.NewRequest("POST", "/api/blackboard/entries", body2)
	w2 := httptest.NewRecorder()
	h.WriteEntry(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("write global: %d %s", w2.Code, w2.Body)
	}

	reqFilter := httptest.NewRequest("GET", "/api/blackboard/entries?workspace=ws-1", nil)
	wFilter := httptest.NewRecorder()
	h.ReadBundle(wFilter, reqFilter)
	if wFilter.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wFilter.Code)
	}
	var bundle Bundle
	if err := json.NewDecoder(wFilter.Body).Decode(&bundle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(bundle.Entries) != 1 {
		t.Fatalf("expected 1 entry (scoped only), got %d", len(bundle.Entries))
	}
	titles := map[string]bool{}
	for _, e := range bundle.Entries {
		titles[e.Title] = true
	}
	if !titles["Scoped entry"] || titles["Global entry"] {
		t.Errorf("expected only scoped entry, got titles: %v", titles)
	}
}
