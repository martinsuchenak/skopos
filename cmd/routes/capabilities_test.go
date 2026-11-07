package routes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	logslog "github.com/paularlott/logger/slog"
)

// A capped key goes through the real middleware and routes: an MCP-only
// agent key is refused on REST (403) but learns its capabilities from
// whoami; a key without capabilities is unaffected.
func TestCappedKeyOverREST(t *testing.T) {
	log := logslog.New(logslog.Config{Level: "error", Writer: io.Discard})
	sqlDB, err := db.Connect(log, filepath.Join(t.TempDir(), "caps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	const rootKey = "root-secret"
	wsSvc := workspaces.NewService(workspaces.NewStorage(sqlDB))
	rootCtx := auth.WithPrincipal(context.Background(), &auth.Principal{Root: true})
	if _, _, err := wsSvc.Create(rootCtx, workspaces.CreateInput{ID: "ws-a"}); err != nil {
		t.Fatal(err)
	}
	keySvc := apikeys.NewService(apikeys.NewStorage(sqlDB))
	agent, err := keySvc.Create(rootCtx, apikeys.CreateInput{Name: "agent", Workspaces: []string{"ws-a"}, Preset: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	full, err := keySvc.Create(rootCtx, apikeys.CreateInput{Name: "full", Workspaces: []string{"ws-a"}})
	if err != nil {
		t.Fatal(err)
	}

	authn := auth.NewAuthenticator(rootKey, apikeys.NewStorage(sqlDB))
	webMux, apiMux := http.NewServeMux(), http.NewServeMux()
	RegisterRoutes(webMux, apiMux,
		status.NewHandler(status.NewService(status.NewStorage(sqlDB)), authn),
		blackboard.NewHandler(blackboard.NewService(blackboard.NewStorage(sqlDB)), authn),
		plans.NewHandler(plans.NewService(plans.NewStorage(sqlDB)), authn),
		inbox.NewHandler(inbox.NewService(inbox.NewStorage(sqlDB)), authn),
		workspaces.NewHandler(wsSvc, authn), nil,
		apikeys.NewHandler(keySvc, wsSvc), nil)
	root := http.NewServeMux()
	root.Handle("/api/", authn.Middleware(apiMux))
	ts := httptest.NewServer(root)
	defer ts.Close()

	get := func(key, path string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(agent.Secret, "/api/blackboard/entries?workspace_id=ws-a"); code != http.StatusForbidden || !strings.Contains(body, "rest") {
		t.Fatalf("agent key on REST: %d %s", code, body)
	}
	code, body := get(agent.Secret, "/api/whoami")
	if code != http.StatusOK {
		t.Fatalf("agent whoami: %d %s", code, body)
	}
	var who struct {
		Key struct {
			Capabilities []string `json:"capabilities"`
		} `json:"key"`
	}
	_ = json.Unmarshal([]byte(body), &who)
	if !reflect.DeepEqual(who.Key.Capabilities, agent.Key.Capabilities) {
		t.Fatalf("whoami capabilities %v, want %v", who.Key.Capabilities, agent.Key.Capabilities)
	}
	if code, body := get(full.Secret, "/api/blackboard/entries?workspace_id=ws-a"); code != http.StatusOK {
		t.Fatalf("full key on REST: %d %s", code, body)
	}
	if code, body := get(full.Secret, "/api/whoami"); code != http.StatusOK || !strings.Contains(body, `"capabilities":null`) {
		t.Fatalf("full key whoami must say capabilities null: %d %s", code, body)
	}
}
