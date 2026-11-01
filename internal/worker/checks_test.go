package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CI loop: passing checks need no round; a failure goes back to the
// implementer until it passes or the rounds run out; a fix round that ends
// the stage (blocked, amendment, pause) stops the loop.
func TestFixChecks(t *testing.T) {
	pass := []Check{{Name: "go test ./...", Passed: true}}
	fail := []Check{{Name: "go vet ./...", Passed: true}, {Name: "go test ./...", Output: "FAIL"}}
	cases := []struct {
		name       string
		max        int
		results    [][]Check // what each run of the checks returns, in order
		stopAt     int       // the fix round that stops the stage (0 = none)
		wantRounds int
		wantPassed bool
		wantStop   bool
	}{
		{"passing", 2, [][]Check{pass}, 0, 0, true, false},
		{"fixed in round 1", 2, [][]Check{fail, pass}, 0, 1, true, false},
		{"fixed in round 2", 2, [][]Check{fail, fail, pass}, 0, 2, true, false},
		{"still failing", 2, [][]Check{fail, fail, fail}, 0, 2, false, false},
		{"no fix rounds", 0, [][]Check{fail}, 0, 0, false, false},
		{"fix round ends the stage", 2, [][]Check{fail}, 1, 1, false, true},
		{"no checks configured", 2, [][]Check{nil}, 0, 0, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runs := 0
			var gotFailed []string
			checks, rounds, stop, _ := fixChecks(c.max,
				func() []Check { runs++; return c.results[runs-1] },
				func(failed []Check, round int) (bool, error) {
					gotFailed = append(gotFailed, checkNames(failed))
					return round == c.stopAt, nil
				})
			if rounds != c.wantRounds || stop != c.wantStop || (len(failedChecks(checks)) == 0) != c.wantPassed {
				t.Fatalf("rounds %d stop %v checks %+v; want rounds %d stop %v passed %v", rounds, stop, checks, c.wantRounds, c.wantStop, c.wantPassed)
			}
			for _, f := range gotFailed {
				if f != "go test ./..." {
					t.Fatalf("the fix round must get only the failed checks, got %q", f)
				}
			}
		})
	}
}

// What the checks test must be what the push sends: uncommitted changes to
// tracked files fail the checks, whether left by the implementer or written
// by a check. Untracked files (build output) don't, and a repo without checks
// runs nothing.
func TestRepoChecksRequireCommittedTree(t *testing.T) {
	dir, _ := gitRepo(t)
	w := &Worker{cfg: &Config{}}
	repo := &Repo{Checks: []string{"true"}}

	got := w.repoChecks(t.Context(), repo, dir)
	if len(got) != 2 || len(failedChecks(got)) != 0 || got[1].Name != committedCheck {
		t.Fatalf("clean tree: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "build.out"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got = w.repoChecks(t.Context(), repo, dir); len(failedChecks(got)) != 0 {
		t.Fatalf("an untracked file must not fail the checks: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("edited, not committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = w.repoChecks(t.Context(), repo, dir)
	failed := failedChecks(got)
	if len(failed) != 1 || failed[0].Name != committedCheck || !strings.Contains(failed[0].Output, "AGENTS.md") {
		t.Fatalf("an uncommitted tracked change must fail %q: %+v", committedCheck, got)
	}
	if got = w.repoChecks(t.Context(), &Repo{}, dir); got != nil {
		t.Fatalf("no checks configured must run nothing: %+v", got)
	}
	// A check that rewrites a tracked file is caught too (it runs first).
	if _, err := git(t.Context(), dir, "checkout", "--", "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	got = w.repoChecks(t.Context(), &Repo{Checks: []string{"echo tidied > AGENTS.md"}}, dir)
	if failed := failedChecks(got); len(failed) != 1 || failed[0].Name != committedCheck {
		t.Fatalf("a check that rewrites a tracked file must fail %q: %+v", committedCheck, got)
	}
}

// The reviewer learns what the worker's checks said about the commit, or
// that nothing tested it, and is told not to report it couldn't run tools.
func TestReviewPromptCarriesChecks(t *testing.T) {
	base := map[string]any{"Base": "main", "Stage": "review", "Steps": []Step{{Title: "s"}}}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		name      string
		data      map[string]any
		want, not []string
	}{
		{"passed", with("Checks", []Check{{Name: "go test ./...", Passed: true}}, "ChecksConfigured", true),
			[]string{"`go test ./...`: passed", "Don't report that you couldn't run them"}, []string{"Each failed check is a blocking finding", "untested"}},
		{"failed", with("Checks", []Check{{Name: "go vet ./...", Output: "vet: bad"}}, "ChecksFailed", true, "ChecksConfigured", true),
			[]string{"`go vet ./...`: FAILED", "vet: bad", "Each failed check is a blocking finding"}, nil},
		{"no results", with("ChecksConfigured", true), []string{"aren't available"}, []string{"untested"}},
		{"no checks configured", with(), []string{"No checks are configured", "untested"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := renderPrompt("review.md", c.data)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range c.want {
				if !strings.Contains(out, s) {
					t.Errorf("prompt lacks %q", s)
				}
			}
			for _, s := range c.not {
				if strings.Contains(out, s) {
					t.Errorf("prompt must not say %q", s)
				}
			}
		})
	}
}

// The fix round hands the implementer each failed check with its output.
func TestFixChecksPrompt(t *testing.T) {
	out, err := renderPrompt("fix_checks.md", map[string]any{
		"Title": "Add Slugify", "Branch": "agent/x", "Base": "main", "Round": 1, "Rounds": 2,
		"Failed":   []Check{{Name: "go test ./...", Output: "--- FAIL: TestSlugify"}},
		"Commands": []string{"git add -A"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"fix round 1 of 2", "`go test ./...`", "--- FAIL: TestSlugify", "Don't weaken, skip or delete", `status "done"`} {
		if !strings.Contains(out, s) {
			t.Errorf("prompt lacks %q", s)
		}
	}
}

func TestCheckFixRoundsValidation(t *testing.T) {
	rounds := func(n int) Config {
		return Config{Repos: map[string]*Repo{"ws": {Path: "/x", CheckFixRounds: &n}}}
	}
	checkConfigCases(t, []configCase{
		{"default", Config{Repos: map[string]*Repo{"ws": {Path: "/x"}}}, ""},
		{"zero blocks at once", rounds(0), ""},
		{"negative", rounds(-1), "check_fix_rounds -1"},
		{"too many", rounds(maxCheckFixRounds + 1), "is not between 0 and"},
	})
	if got := (&Repo{}).FixRounds(); got != defaultCheckFixRounds {
		t.Fatalf("default rounds %d", got)
	}
}
