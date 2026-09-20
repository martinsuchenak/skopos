package inbox

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

func testHandler(t *testing.T) (*Handler, http.Handler) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := NewHandler(NewService(NewStorage(sqlDB)), testAuth("k"))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/inbox", h.CreateItem)
	mux.HandleFunc("GET /api/inbox", h.ListItems)
	mux.HandleFunc("POST /api/inbox/reorder", h.Reorder)
	mux.HandleFunc("GET /api/inbox/{id}", h.GetItem)
	mux.HandleFunc("PATCH /api/inbox/{id}", h.UpdateItem)
	mux.HandleFunc("POST /api/inbox/{id}/claim", h.Claim)
	mux.HandleFunc("POST /api/inbox/{id}/convert", h.Convert)
	mux.HandleFunc("POST /api/inbox/{id}/discard", h.Discard)
	mux.HandleFunc("DELETE /api/inbox/{id}", h.DeleteItem)
	return h, mux
}

type respBody map[string]any

func do(t *testing.T, mux http.Handler, method, path, body string) (int, respBody) {
	t.Helper()
	req, _ := http.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var parsed respBody
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec.Code, parsed
}

func TestHandlerLifecycleEndToEnd(t *testing.T) {
	_, mux := testHandler(t)

	status, body := do(t, mux, "POST", "/api/inbox", `{"workspace_id":"ws-a","title":"Implement user management","content":"rough **markdown**","tags":["Auth","auth","ui"],"author_agent_id":"tester"}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("no id: %v", body)
	}
	tags, _ := body["tags"].([]any)
	if len(tags) != 2 || tags[0] != "auth" || tags[1] != "ui" {
		t.Fatalf("tags should be normalized+deduped: %v", tags)
	}

	// List is a JSON array (never null) with excerpt-only rows.
	req, _ := http.NewRequest("GET", "/api/inbox?workspace_id=ws-a&status=open", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("list should be an array: %v (%s)", err, rec.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("list rows: %d", len(rows))
	}
	if _, has := rows[0]["content"]; has {
		t.Fatalf("list rows should not carry content: %v", rows[0])
	}
	if excerpt, _ := rows[0]["excerpt"].(string); excerpt == "" {
		t.Fatalf("list rows should carry an excerpt: %v", rows[0])
	}

	// Detail: content + rendered HTML.
	status, body = do(t, mux, "GET", "/api/inbox/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, body)
	}
	if html, _ := body["content_html"].(string); html == "" {
		t.Fatalf("detail should render content_html: %v", body)
	}

	// Update (enrichment).
	if status, _ = do(t, mux, "PATCH", "/api/inbox/"+id, `{"content":"enriched"}`); status != http.StatusNoContent {
		t.Fatalf("update: %d", status)
	}

	// Claim + conflict + release.
	if status, _ = do(t, mux, "POST", "/api/inbox/"+id+"/claim", `{"agent_id":"agent-1"}`); status != http.StatusOK {
		t.Fatalf("claim: %d", status)
	}
	if status, _ = do(t, mux, "POST", "/api/inbox/"+id+"/claim", `{"agent_id":"agent-2"}`); status != http.StatusConflict {
		t.Fatalf("claim conflict: %d", status)
	}
	if status, _ = do(t, mux, "POST", "/api/inbox/"+id+"/claim", `{"agent_id":""}`); status != http.StatusOK {
		t.Fatalf("release: %d", status)
	}

	// Convert to a missing plan -> 404.
	if status, _ = do(t, mux, "POST", "/api/inbox/"+id+"/convert", `{"plan_id":"nope"}`); status != http.StatusNotFound {
		t.Fatalf("convert missing plan: %d", status)
	}

	// Discard and delete.
	_, other := do(t, mux, "POST", "/api/inbox", `{"workspace_id":"ws-a","title":"temp","author_agent_id":"t"}`)
	otherID, _ := other["id"].(string)
	if status, _ = do(t, mux, "POST", "/api/inbox/"+otherID+"/discard", `{}`); status != http.StatusNoContent {
		t.Fatalf("discard: %d", status)
	}
	if status, _ = do(t, mux, "DELETE", "/api/inbox/"+otherID, ""); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	if status, _ = do(t, mux, "GET", "/api/inbox/"+otherID, ""); status != http.StatusNotFound {
		t.Fatalf("get deleted: %d", status)
	}

	// Unauthorized without the key.
	req, _ = http.NewRequest("GET", "/api/inbox", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}

	// Reorder: empty ids rejected, then a valid renumber returns 204.
	if status, _ = do(t, mux, "POST", "/api/inbox/reorder", `{"ids":[]}`); status != http.StatusBadRequest {
		t.Fatalf("empty reorder: %d", status)
	}
	_, secondItem := do(t, mux, "POST", "/api/inbox", `{"workspace_id":"ws-a","title":"first","author_agent_id":"t"}`)
	firstID, _ := body["id"].(string)
	secondID, _ := secondItem["id"].(string)
	if status, _ = do(t, mux, "POST", "/api/inbox/reorder", `{"ids":["`+secondID+`","`+firstID+`"]}`); status != http.StatusNoContent {
		t.Fatalf("reorder: %d", status)
	}
	status, _ = do(t, mux, "GET", "/api/inbox?workspace_id=ws-a&status=open", "")
	_ = status

	// Invalid input shapes.
	if status, _ = do(t, mux, "POST", "/api/inbox", `not json`); status != http.StatusBadRequest {
		t.Fatalf("bad body: %d", status)
	}
	if status, _ = do(t, mux, "GET", "/api/inbox?status=bogus", ""); status != http.StatusBadRequest {
		t.Fatalf("bad status filter: %d", status)
	}
	if status, _ = do(t, mux, "POST", "/api/inbox", `{"workspace_id":"ws-a","author_agent_id":"t"}`); status != http.StatusBadRequest {
		t.Fatalf("missing title: %d", status)
	}
}
