package routes

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	logslog "github.com/paularlott/logger/slog"
	_ "modernc.org/sqlite"
)

func integrationSetup(t *testing.T, apiKey string) *http.ServeMux {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("fk: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	log := logslog.New(logslog.Config{Level: "error", Writer: io.Discard})
	rest.SetLogger(log)

	st := status.NewHandler(status.NewService(status.NewStorage(sqlDB)), apiKey)
	bb := blackboard.NewHandler(blackboard.NewService(blackboard.NewStorage(sqlDB)), apiKey)
	pl := plans.NewHandler(plans.NewService(plans.NewStorage(sqlDB)), apiKey)
	ws := workspaces.NewHandler(workspaces.NewService(workspaces.NewStorage(sqlDB)), apiKey)

	mux := http.NewServeMux()
	RegisterRoutes(mux, st, bb, pl, ws)
	hub := events.NewHub()
	mux.HandleFunc("GET /api/events/stream", events.StreamHandler(hub))
	return mux
}

func TestIntegrationHealthAndSessions(t *testing.T) {
	mux := integrationSetup(t, "")
	ts := httptest.NewServer(events.Middleware(events.NewHub(), nil, mux))
	defer ts.Close()

	// health
	resp, _ := http.Get(ts.URL + "/health")
	if resp.StatusCode != 200 {
		t.Fatalf("health: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// empty sessions
	resp, _ = http.Get(ts.URL + "/api/sessions")
	if resp.StatusCode != 200 {
		t.Fatalf("sessions: expected 200, got %d", resp.StatusCode)
	}
	var sessions []status.SessionSummary
	json.NewDecoder(resp.Body).Decode(&sessions)
	resp.Body.Close()
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestIntegrationReportAndBlackboard(t *testing.T) {
	mux := integrationSetup(t, "")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// report a status
	resp, _ := http.Post(ts.URL+"/api/reports", "application/json", strings.NewReader(`{"agent_id":"a1","agent_type":"codex","workspace":"/repo","status":"running"}`))
	if resp.StatusCode != 201 {
		t.Fatalf("report: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// write a blackboard entry
	resp, _ = http.Post(ts.URL+"/api/blackboard/entries", "application/json", strings.NewReader(`{"scope":"project","entry_type":"finding","title":"test finding","author_agent_id":"a1"}`))
	if resp.StatusCode != 201 {
		t.Fatalf("blackboard write: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// read blackboard
	resp, _ = http.Get(ts.URL + "/api/blackboard/entries")
	if resp.StatusCode != 200 {
		t.Fatalf("blackboard read: expected 200, got %d", resp.StatusCode)
	}
	var bundle blackboard.Bundle
	json.NewDecoder(resp.Body).Decode(&bundle)
	resp.Body.Close()
	if len(bundle.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(bundle.Entries))
	}

	// search blackboard
	resp, _ = http.Get(ts.URL + "/api/blackboard/entries?q=finding")
	if resp.StatusCode != 200 {
		t.Fatalf("blackboard search: expected 200, got %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&bundle)
	resp.Body.Close()
	if len(bundle.Entries) != 1 {
		t.Fatalf("search 'finding': expected 1, got %d", len(bundle.Entries))
	}
}

func TestIntegrationPlans(t *testing.T) {
	mux := integrationSetup(t, "")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// create plan
	resp, _ := http.Post(ts.URL+"/api/plans", "application/json", strings.NewReader(`{"name":"Test plan","author_agent_id":"a1"}`))
	if resp.StatusCode != 201 {
		t.Fatalf("create plan: expected 201, got %d", resp.StatusCode)
	}
	var plan plans.Plan
	json.NewDecoder(resp.Body).Decode(&plan)
	resp.Body.Close()

	// add item
	resp, _ = http.Post(ts.URL+"/api/plans/"+plan.ID+"/items", "application/json", strings.NewReader(`{"title":"Item 1"}`))
	if resp.StatusCode != 201 {
		t.Fatalf("add item: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// get plan with items
	resp, _ = http.Get(ts.URL + "/api/plans/" + plan.ID)
	if resp.StatusCode != 200 {
		t.Fatalf("get plan: expected 200, got %d", resp.StatusCode)
	}
	var got plans.Plan
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(got.Items))
	}
}

func TestIntegrationWorkspaceCreateAndList(t *testing.T) {
	mux := integrationSetup(t, "")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// create workspace
	resp, _ := http.Post(ts.URL+"/api/workspaces", "application/json", strings.NewReader(`{"id":"ws-test","name":"Test"}`))
	if resp.StatusCode != 201 {
		t.Fatalf("create workspace: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// list workspaces
	resp, _ = http.Get(ts.URL + "/api/workspaces")
	if resp.StatusCode != 200 {
		t.Fatalf("list workspaces: expected 200, got %d", resp.StatusCode)
	}
	var list []workspaces.Workspace
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0].ID != "ws-test" {
		t.Fatalf("expected 1 workspace 'ws-test', got %+v", list)
	}
}

func TestIntegrationAuthEnforced(t *testing.T) {
	mux := integrationSetup(t, "secret")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// POST without key → 401
	resp, _ := http.Post(ts.URL+"/api/reports", "application/json", strings.NewReader(`{"agent_id":"a","agent_type":"codex","workspace":"/r","status":"running"}`))
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 without key, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// POST with key → 201
	req, _ := http.NewRequest("POST", ts.URL+"/api/reports", strings.NewReader(`{"agent_id":"a","agent_type":"codex","workspace":"/r","status":"running"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "secret")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201 with key, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// DELETE session without key → 401
	req, _ = http.NewRequest("DELETE", ts.URL+"/api/sessions/any", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 on delete without key, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
