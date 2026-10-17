package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/paularlott/cli"
)

type recordedReq struct {
	method, path string
	body         map[string]any
}

// fakeSkopos serves the endpoints the workflow CLI uses and records POSTs.
func fakeSkopos(t *testing.T) (*httptest.Server, *[]recordedReq) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		reqs = append(reqs, recordedReq{r.Method, r.URL.Path, body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/inbox/it-1":
			_, _ = w.Write([]byte(`{"id":"it-1","title":"Add Reverse","status":"awaiting_approval","plan_id":"pl-1","content":"x\n\n## Questions\n\n- runes or bytes?"}`))
		case r.Method == "GET" && r.URL.Path == "/api/plans/pl-1/revisions":
			_, _ = w.Write([]byte(`[{"id":"rev-2","revision_no":2,"base_sha":"abcdef1234567","skills":["go-tests"],"steps":[{"title":"Create reverse.go","description":"by runes","phase":"implement"},{"title":"Review and push (Martin)","phase":"review"}]},{"id":"rev-1","revision_no":1}]`))
		case r.Method == "GET" && r.URL.Path == "/api/inbox/it-2":
			_, _ = w.Write([]byte(`{"id":"it-2","title":"Three ways","status":"awaiting_approval","plan_id":"pl-2","content":"x"}`))
		case r.Method == "GET" && r.URL.Path == "/api/plans/pl-2/revisions":
			// newest first: two open candidates of round g2, one superseded from g1
			_, _ = w.Write([]byte(`[{"id":"c3","revision_no":3,"base_sha":"abc","group_id":"g2","label":"glm","candidate_state":"open","steps":[{"title":"rewrite","phase":"implement"},{"title":"Review and push (Martin)","phase":"review"}]},{"id":"c2","revision_no":2,"base_sha":"abc","group_id":"g2","label":"claude","candidate_state":"open","steps":[{"title":"patch","phase":"implement"}]},{"id":"c1","revision_no":1,"group_id":"g1","label":"old","candidate_state":"superseded"}]`))
		case r.Method == "GET" && r.URL.Path == "/api/runs":
			_, _ = w.Write([]byte(`[{"stage":"review","status":"succeeded"},{"stage":"implement","status":"succeeded","branch":"agent/x","head_sha":"c2b57e43"}]`))
		case r.Method == "POST":
			_, _ = w.Write([]byte(`{"id":"it-1","status":"next"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func runInbox(t *testing.T, args ...string) error {
	t.Helper()
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = append([]string{"skopos", "inbox"}, args...)
	root := &cli.Command{Name: "skopos", Commands: []*cli.Command{inboxCmd(), workspaceCmd()}}
	return root.Execute(context.Background())
}

func lastPost(reqs []recordedReq) recordedReq {
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].method == "POST" {
			return reqs[i]
		}
	}
	return recordedReq{}
}

// Approvals from the CLI name what was reviewed: without a terminal,
// --revision / --head are required; with them, the request binds to them.
func TestInboxWorkflowCLI(t *testing.T) {
	srv, reqs := fakeSkopos(t)
	base := []string{"--server-url", srv.URL, "--api-key", "k", "--id", "it-1"}

	if err := runInbox(t, append([]string{"approve"}, base...)...); err == nil || !strings.Contains(err.Error(), "--revision is required") {
		t.Fatalf("approve without a terminal must require --revision: %v", err)
	}
	if err := runInbox(t, append([]string{"approve", "--revision", "rev-2", "--notes", "use runes"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if p := lastPost(*reqs); p.path != "/api/inbox/it-1/approve" || p.body["revision_id"] != "rev-2" || p.body["notes"] != "use runes" {
		t.Fatalf("approve request: %+v", p)
	}

	if err := runInbox(t, append([]string{"mark-done"}, base...)...); err == nil || !strings.Contains(err.Error(), "--head is required") {
		t.Fatalf("mark-done without a terminal must require --head: %v", err)
	}
	if err := runInbox(t, append([]string{"mark-done", "--head", "c2b57e43"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if p := lastPost(*reqs); p.path != "/api/inbox/it-1/mark-done" || p.body["head_sha"] != "c2b57e43" {
		t.Fatalf("mark-done request: %+v", p)
	}

	if err := runInbox(t, append([]string{"request-changes"}, base...)...); err == nil {
		t.Fatal("request-changes needs --notes")
	}
	if err := runInbox(t, append([]string{"request-changes", "--notes", "split step 1"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if p := lastPost(*reqs); p.path != "/api/inbox/it-1/request-changes" || p.body["notes"] != "split step 1" {
		t.Fatalf("request-changes: %+v", p)
	}
	if err := runInbox(t, append([]string{"retry", "--answer", "runes"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if p := lastPost(*reqs); p.path != "/api/inbox/it-1/retry" || p.body["answer"] != "runes" {
		t.Fatalf("retry: %+v", p)
	}
	if err := runInbox(t, append([]string{"queue"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if p := lastPost(*reqs); p.path != "/api/inbox/it-1/queue" {
		t.Fatalf("queue request: %+v", p)
	}
	if err := runInbox(t, append([]string{"reject", "--yes", "--notes", "duplicate"}, base...)...); err != nil {
		t.Fatal(err)
	}
	if p := lastPost(*reqs); p.path != "/api/inbox/it-1/reject" || p.body["notes"] != "duplicate" {
		t.Fatalf("reject request: %+v", p)
	}
}

// `inbox plan` shows the revision an approval would bind: number, id,
// steps (not the server-owned review step), skills, questions.
func TestInboxPlanShowsRevision(t *testing.T) {
	srv, _ := fakeSkopos(t)
	c := &apiClient{base: srv.URL, key: "k"}
	var out strings.Builder
	rev, _, err := c.showPlan(context.Background(), &out, "it-1")
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Plan revision #2 (rev-2)", "1. Create reverse.go", "by runes", "Skills: go-tests", "runes or bytes?"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Review and push") || rev == nil || rev.ID != "rev-2" {
		t.Fatalf("plan output: %q rev %+v", got, rev)
	}
	run, err := c.latestImplementRun(context.Background(), "it-1")
	if err != nil || run.HeadSHA != "c2b57e43" {
		t.Fatalf("latest implement run: %+v %v", run, err)
	}
}

func TestWorkspaceAdd(t *testing.T) {
	srv, reqs := fakeSkopos(t)
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"skopos", "workspace", "add", "--server-url", srv.URL, "--api-key", "root", "--id", "github.com/you/repo", "--name", "repo"}
	root := &cli.Command{Name: "skopos", Commands: []*cli.Command{workspaceCmd()}}
	if err := root.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := lastPost(*reqs); p.path != "/api/workspaces" || p.body["id"] != "github.com/you/repo" || p.body["name"] != "repo" {
		t.Fatalf("workspace add: %+v", p)
	}
}

// A multi-agent plan gate in the CLI: candidates of the newest round are
// listed oldest first (claude = 1, glm = 2), superseded ones never; and
// request-changes --revision revises one candidate.
func TestInboxPlanShowsCandidates(t *testing.T) {
	srv, reqs := fakeSkopos(t)
	c := &apiClient{base: srv.URL, key: "k"}
	var out strings.Builder
	rev, _, cands, err := c.showPlanFull(context.Background(), &out, "it-2")
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if rev != nil || len(cands) != 2 || cands[0].ID != "c2" || cands[1].ID != "c3" {
		t.Fatalf("candidates: %+v rev %+v", cands, rev)
	}
	for _, want := range []string{"2 plan candidates", "Candidate 1 · claude (c2)", "Candidate 2 · glm (c3)", "1. rewrite"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old") || strings.Contains(got, "Review and push") {
		t.Errorf("superseded candidates and the review step must not show:\n%s", got)
	}
	if err := runInbox(t, "request-changes", "--server-url", srv.URL, "--api-key", "k", "--id", "it-2", "--notes", "split it", "--revision", "c3"); err != nil {
		t.Fatal(err)
	}
	if p := lastPost(*reqs); p.path != "/api/inbox/it-2/request-changes" || p.body["revision_id"] != "c3" || p.body["notes"] != "split it" {
		t.Fatalf("request-changes --revision: %+v", p)
	}
}
