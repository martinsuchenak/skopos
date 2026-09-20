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
	"github.com/martinsuchenak/skopos/internal/inbox"
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

	statusSvc := status.NewService(status.NewStorage(sqlDB))
	blackboardSvc := blackboard.NewService(blackboard.NewStorage(sqlDB))
	plansSvc := plans.NewService(plans.NewStorage(sqlDB))
	inboxSvc := inbox.NewService(inbox.NewStorage(sqlDB))
	workspacesSvc := workspaces.NewService(wsStore)
	statusSvc.SetPublisher(hub)
	blackboardSvc.SetPublisher(hub)
	plansSvc.SetPublisher(hub)
	inboxSvc.SetPublisher(hub)
	workspacesSvc.SetPublisher(hub)

	authn := auth.NewAuthenticator("rootkey", apikeys.NewStorage(sqlDB))
	webMux := http.NewServeMux()
	apiMux := http.NewServeMux()
	RegisterRoutes(webMux, apiMux,
		status.NewHandler(statusSvc, authn),
		blackboard.NewHandler(blackboardSvc, authn),
		plans.NewHandler(plansSvc, authn),
		inbox.NewHandler(inboxSvc, authn),
		workspaces.NewHandler(workspacesSvc, authn),
		nil, nil)
	root := http.NewServeMux()
	root.Handle("/api/", authn.Middleware(apiMux))
	root.Handle("/", webMux)
	ts := httptest.NewServer(events.Middleware(nil, root))
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
	// MCP mutation equivalence: the MCP tool handlers call this same service
	// method with the request context — the publish is identical.
	if _, err := blackboardSvc.Write(context.Background(), blackboard.WriteInput{
		Scope: blackboard.ScopeProject, EntryType: blackboard.TypeFinding,
		Title: "mcp-equivalent", AuthorAgentID: "t", WorkspaceID: "ws-a",
	}); err != nil {
		t.Fatal(err)
	}

	collect()
	byType := map[string]events.Event{}
	for _, ev := range received {
		byType[ev.Type] = ev
	}
	for _, want := range []struct{ typ, ws string }{
		{"sessions", "ws-a"},
		{"blackboard", "ws-a"},
		{"workspaces", "ws-a"},
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
	counts := map[string]int{}
	for _, ev := range received {
		if ev.Workspace != "ws-a" {
			t.Fatalf("unattributed or foreign event leaked: %+v", ev)
		}
		counts[ev.Type]++
	}
	// sessions x1 (report), blackboard x2 (HTTP entry + MCP-equivalent service
	// write), workspaces x1 (HTTP delete), plans x1 (body integrity).
	for typ, n := range map[string]int{"sessions": 1, "blackboard": 2, "workspaces": 1, "plans": 1} {
		if counts[typ] != n {
			t.Fatalf("event %q count %d, want %d (all: %+v)", typ, counts[typ], n, received)
		}
	}
}
