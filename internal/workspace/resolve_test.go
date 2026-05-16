package workspace

import "testing"

func TestNormalizeSSH(t *testing.T) {
	got := Normalize("git@github.com:martinsuchenak/skopos.git")
	want := "github.com/martinsuchenak/skopos"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeHTTPS(t *testing.T) {
	got := Normalize("https://github.com/martinsuchenak/skopos.git")
	want := "github.com/martinsuchenak/skopos"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeSSHProtocol(t *testing.T) {
	got := Normalize("ssh://git@github.com/martinsuchenak/skopos")
	want := "github.com/martinsuchenak/skopos"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeGitLabSubgroup(t *testing.T) {
	got := Normalize("https://gitlab.com/org/subgroup/repo.git")
	want := "gitlab.com/org/subgroup/repo"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeNoGitSuffix(t *testing.T) {
	got := Normalize("https://github.com/martinsuchenak/skopos")
	want := "github.com/martinsuchenak/skopos"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeTrailingSlash(t *testing.T) {
	got := Normalize("https://github.com/martinsuchenak/skopos.git/")
	want := "github.com/martinsuchenak/skopos"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeUpperCase(t *testing.T) {
	got := Normalize("https://GitHub.com/MartinSuchenak/Skopos.git")
	want := "github.com/martinsuchenak/skopos"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeWhitespace(t *testing.T) {
	got := Normalize("  git@github.com:martinsuchenak/skopos.git  ")
	want := "github.com/martinsuchenak/skopos"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeHTTP(t *testing.T) {
	got := Normalize("http://github.com/martinsuchenak/skopos.git")
	want := "github.com/martinsuchenak/skopos"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeEmpty(t *testing.T) {
	got := Normalize("")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestResolveFromGitRepo(t *testing.T) {
	id, err := Resolve(".")
	if err != nil {
		t.Skipf("no git repo: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty workspace ID")
	}
	if id != "github.com/martinsuchenak/skopos" {
		t.Errorf("got %q, want github.com/martinsuchenak/skopos", id)
	}
}

func TestResolveFromNonGitDir(t *testing.T) {
	_, err := Resolve("/tmp/nonexistent-dir-12345")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}
