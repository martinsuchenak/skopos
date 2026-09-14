package cmd

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/cmd/routes"
	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	_ "modernc.org/sqlite"
)



func keysTestServer(t *testing.T, rootKey string) *httptest.Server {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	wsStorage := workspaces.NewStorage(sqlDB)
	if _, _, err := workspaces.NewService(wsStorage).Create(context.Background(), workspaces.CreateInput{ID: "github.com/o/a"}); err != nil {
		t.Fatal(err)
	}

	keyStorage := apikeys.NewStorage(sqlDB)
	handler := apikeys.NewHandler(apikeys.NewService(keyStorage), workspaces.NewService(wsStorage))

	webMux := http.NewServeMux()
	apiMux := http.NewServeMux()
	routes.RegisterRoutes(webMux, apiMux,
		status.NewHandler(status.NewService(status.NewStorage(sqlDB)), testAuth(rootKey)),
		blackboard.NewHandler(blackboard.NewService(blackboard.NewStorage(sqlDB)), testAuth(rootKey)),
		plans.NewHandler(plans.NewService(plans.NewStorage(sqlDB)), testAuth(rootKey)),
		workspaces.NewHandler(workspaces.NewService(wsStorage), testAuth(rootKey)),
		nil, handler)
	root := http.NewServeMux()
	root.Handle("/api/", auth.NewAuthenticator(rootKey, keyStorage).Middleware(apiMux))
	root.Handle("/", webMux)
	ts := httptest.NewServer(root)
	t.Cleanup(ts.Close)
	return ts
}

func TestKeysLifecycleEndToEnd(t *testing.T) {
	ts := keysTestServer(t, "rootkey1")
	ctx := context.Background()

	// Root mints a scoped key.
	var created apikeys.CreateResult
	if err := keysDo(ctx, ts.URL, "rootkey1", http.MethodPost, "/api/keys",
		map[string]any{"name": "ci", "workspaces": []string{"github.com/o/a"}}, &created); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(created.Secret, "sk_") {
		t.Fatalf("bad secret: %s", created.Secret)
	}

	// Management is root-only.
	if err := keysDo(ctx, ts.URL, created.Secret, http.MethodGet, "/api/keys", nil, nil); err == nil {
		t.Fatal("scoped key must not list keys")
	}

	// The minted key authenticates and sees exactly its scope.
	var who struct {
		Root       bool `json:"root"`
		Workspaces []struct {
			ID string `json:"id"`
		} `json:"workspaces"`
	}
	if err := keysDo(ctx, ts.URL, created.Secret, http.MethodGet, "/api/whoami", nil, &who); err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if who.Root || len(who.Workspaces) != 1 || who.Workspaces[0].ID != "github.com/o/a" {
		t.Fatalf("scoped whoami: %+v", who)
	}

	// Revocation cuts access off.
	if err := keysDo(ctx, ts.URL, "rootkey1", http.MethodDelete, "/api/keys/"+created.Key.ID, nil, nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := keysDo(ctx, ts.URL, created.Secret, http.MethodGet, "/api/whoami", nil, &who); err == nil {
		t.Fatal("revoked key must not authenticate")
	}
}

func TestKeyCmdsExist(t *testing.T) {
	if key := keyCmd(); key == nil || key.Name != "key" || len(key.Commands) != 3 {
		t.Fatal("key command must exist with three subcommands")
	}
	if w := whoamiCmd(); w == nil || w.Name != "whoami" {
		t.Fatal("whoami command must exist")
	}
}
