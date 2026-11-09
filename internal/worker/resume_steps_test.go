package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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
	steps := []Step{{Title: "Pin the quote calculation with tests"}, {Title: "Add priceQuote"}, {Title: "Quote write APIs"}}
	if got := committedSteps(t.Context(), dir, base, steps); !reflect.DeepEqual(got, []bool{true, true, false}) {
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
