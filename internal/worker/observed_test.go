package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// changeFacts lists each changed file with its git status and line counts:
// an added, a changed, a deleted, a renamed and a binary file.
func TestChangeFacts(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=a@b", "-c", "user.name=t"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("keep.go", "package a\n\nfunc A() {}\n")
	write("gone.txt", "bye\n")
	write("old name.md", strings.Repeat("line of text that stays\n", 20))
	run("add", ".")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")

	write("keep.go", "package a\n\nfunc A() { B() }\n\nfunc B() {}\n")
	write("db/migrations/002_drop.sql", "DROP TABLE x;\n")
	write("logo.png", "\x89PNG\x00\x01\x02")
	_ = os.Remove(filepath.Join(dir, "gone.txt"))
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("mv", "old name.md", "docs/new name.md")
	run("add", "-A")
	run("commit", "-q", "-m", "change")

	c := changeFacts(context.Background(), dir, base)
	if c == nil {
		t.Fatal("no facts")
	}
	got := map[string]changedFile{}
	for _, f := range c.Files {
		got[f.Path] = f
	}
	want := map[string]changedFile{
		"keep.go":                    {Path: "keep.go", Status: "M", Added: 3, Removed: 1},
		"db/migrations/002_drop.sql": {Path: "db/migrations/002_drop.sql", Status: "A", Added: 1},
		"logo.png":                   {Path: "logo.png", Status: "A", Added: -1, Removed: -1},
		"gone.txt":                   {Path: "gone.txt", Status: "D", Removed: 1},
		"docs/new name.md":           {Path: "docs/new name.md", Status: "R", From: "old name.md"},
	}
	if len(got) != len(want) {
		t.Fatalf("files %+v", c.Files)
	}
	for p, w := range want {
		if got[p] != w {
			t.Fatalf("%s: %+v, want %+v", p, got[p], w)
		}
	}
	if c.Added != 4 || c.Removed != 2 || c.Commits != 1 {
		t.Fatalf("totals: +%d -%d, %d commits", c.Added, c.Removed, c.Commits)
	}
}

// The plan's impact reaches the report as the observed plan facts.
func TestPlanObserved(t *testing.T) {
	pr := planResult{Size: "m", Steps: []Step{{Title: "a"}, {Title: "b"}}, Impact: planImpact{Irreversible: []string{"drops the legacy table"}}}
	plan := planObserved(pr)["plan"].(map[string]any)
	if plan["size"] != "m" || plan["steps"] != 2 || len(plan["irreversible"].([]string)) != 1 {
		t.Fatalf("observed plan: %+v", plan)
	}
}
