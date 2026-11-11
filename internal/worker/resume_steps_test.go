package worker

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// A run cut off mid-implementation (timeout, crash): the next run finds the
// committed steps by title (an amendment renumbered them; a commit-msg hook
// prefixed them), the half-done edits, and a stale index.lock.
func TestResumeAfterCutOffRun(t *testing.T) {
	dir, base := gitRepo(t)
	commit := func(file, msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", msg}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
	}
	commit("a.txt", "agent: step 1 - Document the current API")
	commit("b.txt", "[#0] agent: step 1 - Pin the quote calculation with tests")
	commit("c.txt", "agent: step 2 - Add priceQuote.")
	commit("d.txt", "agent: step 3 - Map GET api/quotes to the handler")
	steps := []Step{{Title: "Pin the quote calculation with tests"}, {Title: "Add priceQuote"}, {Title: "Quote write APIs"},
		{Title: "Map GET /api/quotes to the handler"}}
	if got := committedSteps(t.Context(), dir, base, steps); !reflect.DeepEqual(got, []bool{true, true, false, true}) {
		t.Fatalf("committed: %v", got)
	}

	if got := leftoverChanges(t.Context(), dir); len(got) != 0 {
		t.Fatalf("clean tree: %v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "half.php"), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := leftoverChanges(t.Context(), dir); !reflect.DeepEqual(got, []string{" M a.txt", "?? half.php"}) {
		t.Fatalf("leftover: %q", got)
	}

	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !clearStaleIndexLock(t.Context(), dir) {
		t.Fatal("the stale lock must be removed")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) || clearStaleIndexLock(t.Context(), dir) {
		t.Fatal("no lock, nothing removed")
	}

	steps[0].Done, steps[1].Done = true, true
	out, err := renderPrompt("implement.md", map[string]any{"Title": "T", "Steps": steps, "Resumed": true, "Leftover": []string{"?? half.php"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1. Pin the quote calculation with tests — ALREADY COMMITTED", "3. Quote write APIs\n", "Continue with the first step that is not marked", "?? half.php", "git restore <file>"} {
		if !strings.Contains(out, want) {
			t.Errorf("implement prompt missing %q", want)
		}
	}
	if out, _ := renderPrompt("implement.md", map[string]any{"Title": "T", "Steps": steps[2:]}); strings.Contains(out, "cut off") || strings.Contains(out, "uncommitted") {
		t.Error("a first run gets no resume text")
	}
	// A change-requests round finds every step committed by design.
	if out, _ := renderPrompt("implement.md", map[string]any{"Title": "T", "Steps": steps, "Resumed": true, "Notes": "add the header row"}); strings.Contains(out, "cut off") || !strings.Contains(out, "ALREADY COMMITTED") {
		t.Error("change requests: steps marked, no cut-off text")
	}
}

// The implementer's questions with an amendment or a block are posted to
// the run (they used to be dropped), in both result shapes; a server
// without the endpoint gets the section appended to the item.
func TestImplementerQuestionsWritten(t *testing.T) {
	var ir implResult
	if err := json.Unmarshal([]byte(`{"status":"amendment","reason":"r","questions":[{"question":"New scope?","options":["sales","quotes"]}],"question":"Delete permission?"}`), &ir); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	content := "Item."
	newServer := true
	var posted QuestionsPost
	var lease string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs/run-1/questions":
			if !newServer {
				http.NotFound(w, r)
				return
			}
			lease = r.Header.Get("X-Skopos-Lease")
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet:
			b, _ := json.Marshal(map[string]any{"id": "it", "content": content})
			_, _ = w.Write(b)
		case r.Method == http.MethodPatch:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			content = body["content"]
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "k")
	client.leases = map[string]string{"run-1": "tok"}
	w := &Worker{client: client, cfg: &Config{ID: "w1"}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	run := &Run{ID: "run-1", ItemID: "it", PlanRevID: "rev", BaseSHA: "base"}
	w.appendImplQuestions(t.Context(), run, ir)
	if posted.Asker != "implementer" || !posted.Blocking || posted.SubjectKind != "plan_revision" || posted.Subject != "rev+base" ||
		len(posted.Questions) != 2 || posted.Questions[0].Options[1] != "quotes" || posted.Questions[1].Question != "Delete permission?" || lease != "tok" {
		t.Fatalf("posted: %+v (lease %q)", posted, lease)
	}
	if content != "Item." {
		t.Fatalf("the server renders the section, not the worker: %q", content)
	}
	newServer = false
	w.appendImplQuestions(t.Context(), run, ir)
	want := "## Questions — implementer\n\n1. New scope?\n   - (a) sales\n   - (b) quotes\n2. Delete permission?"
	if !strings.HasSuffix(strings.TrimSpace(content), want) {
		t.Fatalf("fallback content:\n%s", content)
	}
	before := content
	w.appendImplQuestions(t.Context(), run, implResult{Status: "blocked"})
	if content != before {
		t.Fatal("no questions, nothing sent")
	}
}

// Prompts read the decisions from the server's record (with the run's
// lease); a server without it leaves them to the item's sections.
func TestDecisionsFromTheRecord(t *testing.T) {
	record := true
	var lease, worker string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runs/run-1/questions/read" || !record {
			http.NotFound(w, r)
			return
		}
		lease = r.Header.Get("X-Skopos-Lease")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		worker = body["worker_id"]
		_, _ = io.WriteString(w, `{"prompt":"Asked by the planner:\n1. Which backend?\n   Answer: (a) Redis"}`)
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "k")
	client.leases = map[string]string{"run-1": "tok"}
	w := &Worker{client: client, cfg: &Config{ID: "w1"}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, secs := splitItem("Item.\n\n## Questions — planner\n\n1. From the sections?\n\n## Answers\n\nyes\n")
	if got := w.decisions(t.Context(), &Run{ID: "run-1"}, secs); !strings.Contains(got, "Answer: (a) Redis") || lease != "tok" || worker != "w1" {
		t.Fatalf("from the record: %q (lease %q, worker %q)", got, lease, worker)
	}
	record = false
	if got := w.decisions(t.Context(), &Run{ID: "run-1"}, secs); !strings.Contains(got, "From the sections?") {
		t.Fatalf("an older server: from the sections: %q", got)
	}
}
