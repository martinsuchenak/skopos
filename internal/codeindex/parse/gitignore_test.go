package parse

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWalkRespectsGitignore(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(root, ".gitignore"), "build/\n*.min.js\ncache\n!keep.js\n")
	write(t, filepath.Join(root, "src", "app.php"), "<?php class A {}\n")
	write(t, filepath.Join(root, "gen", "nested.php"), "<?php class B {}\n")
	write(t, filepath.Join(root, "assets", "bundle.min.js"), "// packed\n")
	write(t, filepath.Join(root, "cache", "hot.php"), "<?php class C {}\n")
	write(t, filepath.Join(root, "keep.js"), "x\n")

	files, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		got[rel] = true
	}
	if !got["src/app.php"] {
		t.Fatalf("tracked source missing: %v", got)
	}
	if got["build/gen.php"] || got["assets/bundle.min.js"] || got["cache/hot.php"] {
		t.Fatalf("ignored files indexed: %v", got)
	}
	// keep.js is .js — Detect may not index plain js at root anyway; the
	// negation matters when combined with an indexed extension. Assert the
	// matcher directly:
	gi := newGitignoreMatcher()
	gi.loadDir(root)
	if gi.Ignore(root, filepath.Join(root, "build"), true) != true {
		t.Fatal("build/ dir should be ignored")
	}
	if gi.Ignore(root, filepath.Join(root, "keep.js"), false) != false {
		t.Fatal("!keep.js negation should win over *.min.js? no — keep.js is not .min.js; negation still matters for e.g. !keep.php patterns")
	}
}

func TestWalkNoGitignoreOutsideRepo(t *testing.T) {
	root := t.TempDir()
	// no .git dir: .gitignore must NOT apply
	write(t, filepath.Join(root, ".gitignore"), "gen/\n")
	write(t, filepath.Join(root, "gen", "nested.php"), "<?php class B {}\n")
	files, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected gen/nested.php indexed outside a git repo, got %v", files)
	}
}

func TestWalkGitignoreOptOut(t *testing.T) {
	t.Setenv("SKOPOS_NO_GITIGNORE", "1")
	root := t.TempDir()
	write(t, filepath.Join(root, ".git", "HEAD"), "ref\n")
	write(t, filepath.Join(root, ".gitignore"), "gen/\n")
	write(t, filepath.Join(root, "gen", "nested.php"), "<?php class B {}\n")
	files, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("opt-out should index ignored files, got %v", files)
	}
}
