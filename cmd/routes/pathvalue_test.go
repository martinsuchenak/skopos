package routes

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	_ "modernc.org/sqlite"
)

// TestSSEAttributionThroughProductionWiring drives mutations through the real
// middleware order (events wrapping the outer mux, authn wrapping the api
// mux) and asserts the published events carry the caller's workspace from
// every attribution source: query parameter, JSON body, URL path segments,
// and the MCP JSON-RPC envelope.
//
// This replaces an earlier test that wrapped the inner mux directly and so
// validated a wiring production never used — which is how the dead
// PathValue attribution path passed CI (third pentest round, vuln-0004).
func TestSSEAttributionThroughProductionWiring(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db")+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	wsStore := workspaces.NewStorage(sqlDB)
	if _, _, err := workspaces.NewService(wsStore).Create(context.Background(), workspaces.CreateInput{ID: "ws-a"}); err != nil {
		t.Fatal(err)
	}

	hub := events.NewHub()
	eventsCh, unsub := hub.Subscribe()
	defer unsub()

	authn := auth.NewAuthenticator("rootkey", apikeys.NewStorage(sqlDB))
	webMux := http.NewServeMux()
	apiMux := http.NewServeMux()
	RegisterRoutes(webMux, apiMux,
		status.NewHandler(status.NewService(status.NewStorage(sqlDB)), authn),
		blackboard.NewHandler(blackboard.NewService(blackboard.NewStorage(sqlDB)), authn),
		plans.NewHandler(plans.NewService(plans.NewStorage(sqlDB)), authn),
		workspaces.NewHandler(workspaces.NewService(wsStore), authn),
		nil, nil)
	root := http.NewServeMux()
	root.Handle("/api/", authn.Middleware(apiMux))
	root.Handle("/", webMux)
	root.Handle("/mcp", authn.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain like the real MCP handler does — proving the pre-handler
		// peek spliced the body back intact.
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	})))
	ts := httptest.NewServer(events.Middleware(hub, nil, root))
	defer ts.Close()

	post := func(path, body string) {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer rootkey")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	var received []events.Event
	collect := func() {
		for {
			select {
			case ev := <-eventsCh:
				received = append(received, ev)
			default:
				return
			}
		}
	}

	// Body attribution (the pentest's primary case: workspace only in JSON).
	post("/api/reports", `{"agent_id":"a","agent_type":"zcode","workspace":"ws-a","status":"running"}`)
	// Query-parameter attribution.
	post("/api/blackboard/entries?workspace=ws-a", `{"scope":"project","entry_type":"finding","title":"t","author_agent_id":"a","workspace_id":"ws-a"}`)
	// Path-segment attribution via the workspaces route (DELETE).
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/workspaces/ws-a", nil)
	req.Header.Set("Authorization", "Bearer rootkey")
	resp, _ := http.DefaultClient.Do(req)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// MCP envelope attribution.
	post("/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blackboard_read","arguments":{"workspace_id":"ws-a"}}}`)

	collect()
	byType := map[string]events.Event{}
	for _, ev := range received {
		byType[ev.Type] = ev
	}
	for _, want := range []struct{ typ, ws string }{
		{"sessions", "ws-a"},
		{"blackboard", "ws-a"},
		{"workspaces", "ws-a"},
		{"change", "ws-a"},
	} {
		got := byType[want.typ]
		if got.Workspace != want.ws {
			t.Errorf("event %q attributed %q, want %q (all events: %+v)", want.typ, got.Workspace, want.ws, received)
		}
	}

	// Body integrity through the pre-handler peek: the handler must still
	// see the full valid payload (a 400 here would mean the splice broke it).
	post("/api/plans", `{"name":"integrity","author_agent_id":"a","workspace_id":"ws-a"}`)
	collect()
	found := false
	for _, ev := range received {
		if ev.Type == "plans" && ev.Workspace == "ws-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("plans mutation missing or unattributed: %+v", received)
	}
}
