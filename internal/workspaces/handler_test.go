package workspaces

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHandlerCreateAndList(t *testing.T) {
	h := testHandler(t, "")

	body := strings.NewReader(`{"id":"github.com/you/repo","name":"My Repo"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Create(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", w.Code, w.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	w2 := httptest.NewRecorder()
	h.List(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w2.Code)
	}
	var list []Workspace
	if err := json.NewDecoder(w2.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].ID != "github.com/you/repo" || list[0].Name != "My Repo" {
		t.Fatalf("unexpected list: %+v", list)
	}
}

func TestHandlerCreateRejectsEmptyID(t *testing.T) {
	h := testHandler(t, "")
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(`{"id":"  "}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Create(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty id, got %d", w.Code)
	}
}

func TestHandlerCreateUpsertsName(t *testing.T) {
	h := testHandler(t, "")
	ctx := context.Background()
	if _, err := h.service.Create(ctx, CreateInput{ID: "ws1", Name: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Create(ctx, CreateInput{ID: "ws1", Name: "new"}); err != nil {
		t.Fatal(err)
	}
	list, err := h.service.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "new" {
		t.Fatalf("upsert should update name, got %+v", list)
	}
}

func TestHandlerDeleteNotFound(t *testing.T) {
	h := testHandler(t, "")
	req := httptest.NewRequest(http.MethodDelete, "/api/workspaces/nope", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	h.Delete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandlerCreateRequiresAuth(t *testing.T) {
	h := testHandler(t, "secret")
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(`{"id":"ws1"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Create(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}
