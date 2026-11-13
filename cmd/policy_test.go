package cmd

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/cmd/routes"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/paularlott/cli"
)

// `skopos policy set|show|clear` at each level: rules are parsed from
// --allow, a workspace's show lists every policy that applies with where it
// comes from, and exactly one owner is named.
func TestPolicyCLI(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO workspaces (id, created_at) VALUES ('fabrikam/api', '');
		INSERT INTO workspace_groups (id, name, created_at) VALUES ('g1', 'globex', '');
		INSERT INTO workspace_group_members (group_id, workspace_id) VALUES ('g1', 'fabrikam/api')`); err != nil {
		t.Fatal(err)
	}
	svc := pipeline.NewService(pipeline.NewStorage(sqlDB))
	svc.SetRevisions(confrev.NewStore(sqlDB))
	authn := auth.NewAuthenticator("root-key", nil)
	mux := http.NewServeMux()
	routes.RegisterPipelineRoutes(mux, pipeline.NewHandler(svc, authn))
	srv := httptest.NewServer(authn.Middleware(mux))
	t.Cleanup(srv.Close)

	run := func(args ...string) (string, error) {
		t.Helper()
		oldArgs, oldOut := os.Args, os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		defer func() { os.Args, os.Stdout = oldArgs, oldOut }()
		os.Args = append(append([]string{"skopos", "policy"}, args...), "--server-url", srv.URL, "--api-key", "root-key")
		err := (&cli.Command{Name: "skopos", Commands: []*cli.Command{policyCmd()}}).Execute(context.Background())
		_ = w.Close()
		out, _ := io.ReadAll(r)
		return string(out), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}

	if out := must("show", "--workspace", "fabrikam/api"); !strings.Contains(out, "no policy applies") {
		t.Fatalf("nothing set yet: %s", out)
	}
	must("set", "--fleet", "ceiling", "--allow", "provider=anthropic", "--allow", "provider=openai", "--allow", "provider=local")
	out := must("set", "--group", "globex", "--allow", "provider=anthropic", "--allow", "provider=openai,stages=second_opinion")
	if !strings.Contains(out, "saved group globex") || !strings.Contains(out, "allow provider=openai,stages=second_opinion") || !strings.Contains(out, "set by root") {
		t.Fatalf("set output: %s", out)
	}
	out = must("show", "--workspace", "fabrikam/api")
	if !strings.Contains(out, "fleet ceiling (revision ") || !strings.Contains(out, "group globex (revision ") ||
		strings.Index(out, "fleet ceiling") > strings.Index(out, "group globex") || !strings.Contains(out, "    allow provider=local") {
		t.Fatalf("a workspace shows every policy that applies, the ceiling first: %s", out)
	}
	if out := must("show", "--group", "globex"); !strings.Contains(out, "group globex:") || !strings.Contains(out, "allow provider=anthropic") {
		t.Fatalf("group show: %s", out)
	}
	if out := must("show", "--fleet", "default"); !strings.Contains(out, "no policy at this level") {
		t.Fatalf("fleet default is not set: %s", out)
	}
	must("clear", "--group", "globex")
	if out := must("show", "--workspace", "fabrikam/api"); strings.Contains(out, "group globex") || !strings.Contains(out, "fleet ceiling") {
		t.Fatalf("after clearing the group policy: %s", out)
	}

	for name, args := range map[string][]string{
		"no rules":      {"set", "--workspace", "fabrikam/api"},
		"a bad rule":    {"set", "--workspace", "fabrikam/api", "--allow", "vendor=x"},
		"two owners":    {"set", "--workspace", "fabrikam/api", "--group", "globex", "--allow", "any"},
		"no owner":      {"clear"},
		"a bad level":   {"show", "--fleet", "floor"},
		"unknown group": {"set", "--group", "nope", "--allow", "any"},
		"unknown stage": {"set", "--workspace", "fabrikam/api", "--allow", "stages=deploy"},
	} {
		if _, err := run(args...); err == nil {
			t.Errorf("%s: must fail", name)
		}
	}
}
