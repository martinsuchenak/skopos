package cmd

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/paularlott/cli"
)

// `skopos workspace|group pipeline set|clear` edit one stage at a time:
// each call reads the stored settings, changes only what was asked and
// writes the whole document back.
func TestPipelineCLI(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO workspaces (id, created_at) VALUES ('fx/api', ''); INSERT INTO workspace_groups (id, name, created_at) VALUES ('g1', 'fortix', '')`); err != nil {
		t.Fatal(err)
	}
	svc := pipeline.NewService(pipeline.NewStorage(sqlDB))
	h := pipeline.NewHandler(svc, auth.NewAuthenticator("root-key", nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/workspaces/{id}/pipeline", h.GetWorkspace)
	mux.HandleFunc("PUT /api/workspaces/{id}/pipeline", h.PutWorkspace)
	mux.HandleFunc("GET /api/groups/{id}/pipeline", h.GetGroup)
	mux.HandleFunc("PUT /api/groups/{id}/pipeline", h.PutGroup)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	run := func(args ...string) error {
		t.Helper()
		old := os.Args
		defer func() { os.Args = old }()
		os.Args = append(append([]string{"skopos"}, args...), "--server-url", srv.URL, "--api-key", "root-key")
		root := &cli.Command{Name: "skopos", Commands: []*cli.Command{workspaceCmd(), groupCmd()}}
		return root.Execute(context.Background())
	}
	ws := []string{"workspace", "pipeline"}
	must := func(args ...string) {
		t.Helper()
		if err := run(args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	must(append(ws, "set", "--workspace", "fx/api", "--stage", "review", "--runners", "security@claude,tests@opencode-glm", "--policy", "majority", "--timeout", "45m")...)
	must(append(ws, "set", "--workspace", "fx/api", "--stage", "plan", "--runners", "claude")...)
	must(append(ws, "set", "--workspace", "fx/api", "--agents-default", "solo")...)
	v, err := svc.Get(context.Background(), pipeline.KindWorkspace, "fx/api")
	if err != nil {
		t.Fatal(err)
	}
	rev := v.Own.Stages["review"]
	if pipeline.FormatMembers(rev.Members) != "security@claude,tests@opencode-glm" || rev.Policy != "majority" || rev.TimeoutMS != 45*60000 ||
		pipeline.FormatMembers(v.Own.Stages["plan"].Members) != "claude" || v.Own.AgentsDefault != "solo" {
		t.Fatalf("set merges one stage at a time: %+v", v.Own)
	}
	must(append(ws, "clear", "--workspace", "fx/api", "--stage", "review")...)
	v, _ = svc.Get(context.Background(), pipeline.KindWorkspace, "fx/api")
	if _, ok := v.Own.Stages["review"]; ok || len(v.Own.Stages) != 1 || v.Own.AgentsDefault != "solo" {
		t.Fatalf("clear removes only that stage: %+v", v.Own)
	}
	if err := run(append(ws, "set", "--workspace", "fx/api", "--stage", "implement", "--runners", "a,b")...); err == nil {
		t.Fatal("a list on implement must be refused by the server")
	}
	if err := run(append(ws, "set", "--workspace", "fx/api")...); err == nil {
		t.Fatal("set with nothing to set must fail")
	}
	must(append(ws, "show", "--workspace", "fx/api")...)
	must("group", "pipeline", "set", "--group", "fortix", "--stage", "qa", "--runners", "claude")
	g, err := svc.Get(context.Background(), pipeline.KindGroup, "fortix")
	if err != nil || pipeline.FormatMembers(g.Own.Stages["qa"].Members) != "claude" {
		t.Fatalf("group pipeline set: %+v %v", g, err)
	}
	must("group", "pipeline", "clear", "--group", "fortix", "--all")
	if g, _ = svc.Get(context.Background(), pipeline.KindGroup, "fortix"); len(g.Own.Stages) != 0 {
		t.Fatalf("clear --all: %+v", g.Own)
	}
}
