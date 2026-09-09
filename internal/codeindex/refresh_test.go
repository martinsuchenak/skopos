package codeindex

import "testing"

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
		"/local/path/repo",
		"file:///repo",
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
