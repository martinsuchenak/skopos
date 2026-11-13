package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/findings"
)

func refsFor(es ...findings.Entry) []findingRef { return findingRefs(es) }

// An agent answers findings by the handle its prompt gave them. What it
// writes is matched loosely (case, quoting) and filtered strictly: only
// handles the prompt had, only the statuses the stage gives, once each.
func TestResolveAnswers(t *testing.T) {
	refs := refsFor(
		findings.Entry{ID: "id-1", Severity: "blocking", Text: "a"},
		findings.Entry{ID: "id-2", Severity: "nit", Text: "b"},
		findings.Entry{ID: "id-3", Severity: "blocking", Text: "c"},
	)
	if refs[0].Handle != "F1" || refs[2].Handle != "F3" {
		t.Fatalf("handles: %+v", refs)
	}
	got := resolveAnswers(refs, []findingAnswer{
		{ID: " `f1` ", Status: "addressed", Note: " fixed in the handler "},
		{ID: "F2", Status: "still_open"},
		{ID: "F3", Status: "fixed"},     // the implementer's word, not a reviewer's
		{ID: "F9", Status: "addressed"}, // not in the prompt
		{ID: "id-3", Status: "addressed"},
		{ID: "F1", Status: "still_open"}, // a second answer
	}, findings.KindAddressed, findings.KindStillOpen)
	if len(got) != 2 || got[0].Ref.ID != "id-1" || got[0].Note != "fixed in the handler" || got[1].Ref.ID != "id-2" {
		t.Fatalf("resolved: %+v", got)
	}
	wire := wireAnswers(got)
	if len(wire) != 2 || wire[0].ID != "id-1" || wire[0].Status != "addressed" || wire[1].Status != "still_open" {
		t.Fatalf("the report carries the record's ids: %+v", wire)
	}
	// A nit found still open never blocks; a blocking one does.
	if n := stillOpenBlocking(got); n != 0 {
		t.Fatalf("a still-open nit does not block: %d", n)
	}
	blocking := resolveAnswers(refs, []findingAnswer{{ID: "F3", Status: "still_open"}}, findings.KindAddressed, findings.KindStillOpen)
	if n := stillOpenBlocking(blocking); n != 1 {
		t.Fatalf("a still-open blocking finding blocks: %d", n)
	}
}

// An earlier blocking finding that is still open fails the review without
// a finding of its own, and without the summary being turned into one.
func TestReviewNormaliseWithStillOpen(t *testing.T) {
	rr := reviewResult{Verdict: "pass", Summary: "the race is still there"}
	rr.normaliseWith(1)
	if rr.Verdict != "fail" || len(rr.Findings) != 0 {
		t.Fatalf("still open fails it: %q %+v", rr.Verdict, rr.Findings)
	}
	rr = reviewResult{Verdict: "fail", Summary: "the race is still there"}
	rr.normaliseWith(1)
	if rr.Verdict != "fail" || len(rr.Findings) != 0 {
		t.Fatalf("no finding is made up from the summary: %+v", rr.Findings)
	}
	rr = reviewResult{Verdict: "fail", Summary: "s", Findings: []Finding{{Severity: "nit", Text: "x"}}}
	rr.normaliseWith(0)
	if rr.Verdict != "pass" {
		t.Fatalf("nothing still open, nits only: %q", rr.Verdict)
	}
	// What it said about earlier findings is in the item's section.
	sec := reviewSection(reviewResult{Verdict: "fail", Summary: "one left"}, []answered{
		{Ref: findingRef{Entry: findings.Entry{Text: "counts per process", By: "review"}}, Status: findings.KindAddressed, Note: "shared now"},
		{Ref: findingRef{Entry: findings.Entry{Text: "race in the counter", By: "second opinion · codex"}}, Status: findings.KindStillOpen},
	})
	for _, want := range []string{"**addressed** (raised by review) counts per process — shared now", "**still open** (raised by second opinion · codex) race in the counter"} {
		if !strings.Contains(sec, want) {
			t.Fatalf("section lacks %q:\n%s", want, sec)
		}
	}
}

// The prompts list earlier findings under handles and ask for an answer to
// each; without any, they read as before.
func TestPromptsCarryFindings(t *testing.T) {
	toFix := refsFor(
		findings.Entry{ID: "id-1", Severity: "blocking", Text: "counts per process", File: "limit.go:41", By: "review"},
		findings.Entry{ID: "id-2", Severity: "blocking", Text: "race in the counter", By: "second opinion · codex", ReviewNote: "two statements, no transaction"},
	)
	impl, err := renderPrompt("implement.md", map[string]any{"Title": "T", "Findings": toFix})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Review findings to fix (answer every one)",
		"- `F1` (blocking, raised by review, `limit.go:41`): counts per process",
		"- `F2` (blocking, raised by second opinion · codex): race in the counter",
		"A reviewer checked it again and it was not fixed: two statements, no transaction",
		`"fixed" or "not_fixed"`,
		"The `F` numbers exist only in this prompt.",
	} {
		if !strings.Contains(impl, want) {
			t.Fatalf("implement prompt lacks %q:\n%s", want, impl)
		}
	}
	if strings.Contains(impl, "id-1") {
		t.Fatalf("the prompt names findings by handle, never by id:\n%s", impl)
	}
	if plain, _ := renderPrompt("implement.md", map[string]any{"Title": "T"}); strings.Contains(plain, "Review findings") {
		t.Fatalf("no findings, no section:\n%s", plain)
	}

	recheck := refsFor(findings.Entry{ID: "id-1", Severity: "blocking", Text: "counts per process", By: "review", Claim: findings.KindFixed, ClaimNote: "the counter is in the database"},
		findings.Entry{ID: "id-2", Severity: "blocking", Text: "race in the counter", By: "second opinion · codex", Claim: findings.KindNotFixed})
	others := refsFor(findings.Entry{ID: "id-3", Severity: "blocking", Text: "no test for the limit", By: "review · tests"})
	waived := refsFor(findings.Entry{ID: "id-4", Severity: "blocking", Text: "no limit for admins", WaiveNote: "admins are trusted"})
	review, err := renderPrompt("review.md", map[string]any{"Base": "main", "Recheck": recheck, "OthersCheck": others, "Waived": waived})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Earlier findings to check again (answer every one)",
		"- `F1` (blocking, raised by review): counts per process",
		"The implementer says: fixed. the counter is in the database",
		"The implementer says: not fixed",
		"# Findings another reviewer checks",
		"(blocking, raised by review · tests) no test for the limit",
		"# Findings the approver accepted",
		"- no limit for admins (the approver: admins are trusted)",
		"or an earlier blocking finding is",
		"nobody reported before",
		"- `previous`: one entry for every earlier finding listed above",
		"The `F` numbers exist only in this prompt.",
	} {
		if !strings.Contains(review, want) {
			t.Fatalf("review prompt lacks %q:\n%s", want, review)
		}
	}
	plain, _ := renderPrompt("review.md", map[string]any{"Base": "main"})
	for _, not := range []string{"Earlier findings", "`previous`", "another reviewer checks", "approver accepted", "nobody reported before", "`F` numbers"} {
		if strings.Contains(plain, not) {
			t.Fatalf("no earlier findings, but the prompt says %q:\n%s", not, plain)
		}
	}
	if !strings.Contains(plain, `"fail" if there is at least one blocking finding, otherwise "pass"`) {
		t.Fatalf("the plain verdict line changed:\n%s", plain)
	}
}

// The schemas take the answers, and stay valid JSON.
func TestSchemasTakeFindingAnswers(t *testing.T) {
	for name, schema := range map[string]string{"review": reviewSchema, "implement": implSchema} {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal([]byte(schema), &s); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		field := map[string]string{"review": "previous", "implement": "findings"}[name]
		if _, ok := s.Properties[field]; !ok {
			t.Fatalf("%s schema has no %q", name, field)
		}
		// Optional: a run with nothing to answer returns none.
		for _, r := range s.Required {
			if r == field && name == "review" {
				t.Fatalf("%s schema requires %q", name, field)
			}
		}
	}
	var rr reviewResult
	if err := json.Unmarshal([]byte(`{"verdict":"pass","summary":"s","findings":null,"previous":null}`), &rr); err != nil || rr.Previous != nil {
		t.Fatalf("a strict-schema runner returns null for what it has not: %v %+v", err, rr)
	}
}

// A server from before the record answers 404: the worker carries on
// without it. A read that fails otherwise is an error the stage notes.
func TestReadFindings(t *testing.T) {
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runs/r1/findings/read" || r.Header.Get("X-Skopos-Lease") == "" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusTeapot)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		_ = json.NewEncoder(w).Encode(findings.View{Fix: []findings.Entry{{ID: "id-1", Severity: "blocking", Text: "counts per process", By: "review"}}})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "k")
	c.mu.Lock()
	c.leases["r1"] = "lease"
	c.mu.Unlock()
	ctx := context.Background()

	status = http.StatusOK
	view, ok, err := c.ReadFindings(ctx, "r1", "w1")
	if err != nil || !ok || len(view.Fix) != 1 || view.Fix[0].ID != "id-1" {
		t.Fatalf("read: %+v %v %v", view, ok, err)
	}
	status = http.StatusNotFound
	if view, ok, err := c.ReadFindings(ctx, "r1", "w1"); err != nil || ok || view != nil {
		t.Fatalf("an older server: %+v %v %v", view, ok, err)
	}
	status = http.StatusInternalServerError
	if _, ok, err := c.ReadFindings(ctx, "r1", "w1"); err == nil || ok {
		t.Fatalf("a failing read is an error: %v %v", ok, err)
	}
}

// The implement stage end to end with a fake implementer: the prompt lists
// the findings the record says are to fix, under handles, and the run's
// report carries the implementer's answers under the record's ids.
func TestImplementAnswersFindings(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir, base := gitRepo(t)
	if out, err := exec.Command("git", "-C", dir, "update-ref", "refs/remotes/origin/main", base).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v %s", err, out)
	}
	promptFile := filepath.Join(t.TempDir(), "prompt")
	// F2 is answered in a form of its own, F3 was never in the prompt.
	implementer := `prompt="$2"
printf '%s' "$prompt" > "$PROMPT"
echo good > state; git add -A; git -c user.name=t -c user.email=t@t commit -qm "agent: step 1"
printf '{"type":"result","session_id":"s1","structured_output":{"status":"done","steps_done":[1],"summary":"fixed","findings":[{"id":"F1","status":"fixed","note":"moved the counter"},{"id":"f2","status":"not_fixed","note":"needs a schema change"},{"id":"F3","status":"fixed","note":"x"}]}}'
`
	var mu sync.Mutex
	var reports []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/inbox/item-1":
			_, _ = io.WriteString(w, `{"id":"item-1","workspace_id":"ws","title":"Rate limit","content":"c","plan_id":"plan-1","status":"implementing"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/plans/plan-1":
			_, _ = io.WriteString(w, `{"id":"plan-1","items":[{"title":"Write the state file","description":"d"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs/run-1/findings/read":
			_ = json.NewEncoder(w).Encode(findings.View{Fix: []findings.Entry{
				{ID: "rec-a", Severity: "blocking", Text: "counts per process", By: "review", File: "limit.go:41"},
				{ID: "rec-b", Severity: "blocking", Text: "race in the counter", By: "second opinion · codex"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs/run-1/heartbeat":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs/run-1/report":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			reports = append(reports, body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	repo := &Repo{Path: dir, DefaultBranch: "main", BranchPrefix: "agent/"}
	repo.Push.Kind, repo.Push.PullRequest = "none", "none"
	cfg := &Config{
		ID: "worker-test", Bin: fakeCLI(t, implementer), StateDir: t.TempDir(), ImplTimeout: time.Minute,
		Repos:    map[string]*Repo{"ws": repo},
		Accounts: []*Account{{Name: "a1", Runner: "claude", Env: map[string]string{"PROMPT": promptFile}}},
	}
	w := New(cfg, NewClient(srv.URL, "k"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	run := &Run{ID: "run-1", WorkspaceID: "ws", ItemID: "item-1", Stage: "implement", BaseSHA: base}
	if err := w.stageImplement(t.Context(), run, w.log); err != nil {
		t.Fatal(err)
	}
	prompt, _ := os.ReadFile(promptFile)
	for _, want := range []string{"- `F1` (blocking, raised by review, `limit.go:41`): counts per process", "- `F2` (blocking, raised by second opinion · codex): race in the counter"} {
		if !strings.Contains(string(prompt), want) {
			t.Fatalf("the implementer's prompt lacks %q:\n%s", want, prompt)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 || reports[0]["status"] != "succeeded" {
		t.Fatalf("reports: %v", reports)
	}
	raw, _ := json.Marshal(reports[0]["finding_answers"])
	if string(raw) != `[{"id":"rec-a","note":"moved the counter","status":"fixed"},{"id":"rec-b","note":"needs a schema change","status":"not_fixed"}]` {
		t.Fatalf("the report carries the answers under the record's ids: %s", raw)
	}
}

// Answers are an extra on a run's result: whatever shape an agent gives
// them, the result still parses and still passes the schema a non-native
// runner's output is checked against. What can be read is matched loosely.
func TestFindingAnswersNeverFailAResult(t *testing.T) {
	refs := refsFor(findings.Entry{ID: "id-1", Severity: "blocking"}, findings.Entry{ID: "id-2", Severity: "blocking"})
	for name, raw := range map[string]string{
		"a string":            `{"verdict":"pass","summary":"s","previous":"none"}`,
		"an object":           `{"verdict":"pass","summary":"s","previous":{"F1":"addressed"}}`,
		"null":                `{"verdict":"pass","summary":"s","previous":null}`,
		"entries of any kind": `{"verdict":"pass","summary":"s","previous":["F1",7,{"id":2,"status":"Still Open"},{"id":"F1"},{"id":"f1","status":"ADDRESSED","note":5}]}`,
	} {
		var rr reviewResult
		if err := json.Unmarshal([]byte(raw), &rr); err != nil || rr.Verdict != "pass" {
			t.Fatalf("%s: the result must still parse: %v %+v", name, err, rr)
		}
		if name != "entries of any kind" {
			if len(rr.Previous) != 0 {
				t.Fatalf("%s: nothing to read: %+v", name, rr.Previous)
			}
			continue
		}
		said := resolveAnswers(refs, rr.Previous, findings.KindAddressed, findings.KindStillOpen)
		if len(said) != 2 || said[0].Ref.ID != "id-2" || said[0].Status != findings.KindStillOpen || said[1].Ref.ID != "id-1" || said[1].Status != findings.KindAddressed {
			t.Fatalf("read loosely: a number for a handle, a status in words: %+v", said)
		}
	}
	var ir implResult
	if err := json.Unmarshal([]byte(`{"status":"done","steps_done":[1],"summary":"s","findings":[{"id":"F1","status":"not_fixed"}]}`), &ir); err != nil || len(ir.Findings) != 1 {
		t.Fatalf("an answer without a note: %v %+v", err, ir.Findings)
	}
	// The schemas a non-native runner's output is validated against take an
	// entry without a note and a status of the agent's own.
	for name, c := range map[string]struct{ schema, result string }{
		"review":    {reviewSchema, `{"verdict":"pass","summary":"s","previous":[{"id":"F1","status":"partially_fixed"},{"id":"F2"}]}`},
		"implement": {implSchema, `{"status":"done","steps_done":[1],"summary":"s","findings":[{"id":"F1","status":"not_fixed"}]}`},
	} {
		var v any
		if err := json.Unmarshal([]byte(c.result), &v); err != nil {
			t.Fatal(err)
		}
		if err := validateSchema(v, c.schema); err != nil {
			t.Fatalf("%s: an odd answer must not fail the result: %v", name, err)
		}
	}
}
