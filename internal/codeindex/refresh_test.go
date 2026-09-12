package codeindex

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidBranch(t *testing.T) {
	for _, ok := range []string{"main", "feat/x", "release-1.2", "a_b.c"} {
		if !validBranch(ok) {
			t.Errorf("validBranch(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"--upload-pack=evil", "-dash", "a..b", "a b", "a~b", "a^b", "a:b", "evil\nx", "",
	} {
		if validBranch(bad) {
			t.Errorf("validBranch(%q) = true, want false", bad)
		}
	}
}

func TestSafeGitURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/org/repo.git",
		"http://host/repo",
		"git@github.com:org/repo.git",
		"ssh://git@host/repo",
		"git://host/repo",
		"relative/path/repo",
		"./repo",
	} {
		if !safeGitURL(ok) {
			t.Errorf("safeGitURL(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"ext::sh -c touch /tmp/pwned", // command execution transport
		"fd::17",
		"ftp://host/repo",
		"gopher://host",
		"file:///repo",
		"/local/path/repo",
		"../outside/repo",
		"a/../../escape",
		"~/repo",     // git expands ~ to $HOME — escapes the working tree
		"~user/repo", // git expands ~user to that user's home
		"../repo",
		"a/../b/../../c",
	} {
		if safeGitURL(bad) {
			t.Errorf("safeGitURL(%q) = true, want false", bad)
		}
	}
}

func TestSlugOfNoCollisions(t *testing.T) {
	ids := []string{"foo/bar", "foo-bar", "foo_bar", "foo.bar", "FOO/BAR"}
	seen := map[string]string{}
	for _, id := range ids {
		slug := slugOf(id)
		if prev, dup := seen[slug]; dup {
			t.Fatalf("slugOf collision: %q and %q both -> %q", prev, id, slug)
		}
		seen[slug] = id
	}
	// Unsanitized ids keep their plain slug (stable file names).
	if slugOf("simple") != "simple" {
		t.Fatalf("plain slug changed: %q", slugOf("simple"))
	}
}

func TestSlugOfNeverDotOnly(t *testing.T) {
	// The slug is joined into the index/checkout directories; a slug of "."
	// or ".." would escape them (up to and including deleting the whole
	// index directory on failed-clone cleanup). Inputs that sanitize down
	// to a dot-only slug get a digest-derived ws- name instead.
	for _, id := range []string{".", "..", "./", "../", "/..", ":..", "-.."} {
		slug := slugOf(id)
		if slug == "." || slug == ".." {
			t.Errorf("slugOf(%q) = %q — traverses the index dir", id, slug)
		}
		if !strings.HasPrefix(slug, "ws-") {
			t.Errorf("slugOf(%q) = %q — expected digest-derived ws- name", id, slug)
		}
	}
	// Stable across calls (same input, same digest).
	if slugOf("..") != slugOf("..") {
		t.Error("dot-slug substitution is not deterministic")
	}
	// Property: for adversarial inputs the joined path stays inside the dir.
	dir := t.TempDir()
	for _, id := range []string{"", ".", "..", "../..", "a/../..", "...", "~", "a b", "\n"} {
		joined := filepath.Join(dir, slugOf(id))
		if rel, err := filepath.Rel(dir, joined); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Errorf("slugOf(%q) = %q escapes %s (rel %q)", id, slugOf(id), dir, rel)
		}
	}
	// Regular dot-containing ids are unaffected.
	if slugOf("foo.bar") != "foo.bar" {
		t.Errorf("slugOf(foo.bar) = %q", slugOf("foo.bar"))
	}
}
