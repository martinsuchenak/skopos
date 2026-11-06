package build

import "testing"

func TestVersionDefined(t *testing.T) {
	if Version == "" {
		t.Error("Version should not be empty")
	}
}

func TestDateDefined(t *testing.T) {
	if Date == "" {
		t.Error("Date should not be empty")
	}
}

// The worker's self-upgrade accepts a downloaded binary only when its
// --version output contains the version it expected.
func TestStringNamesVersionCommitAndDate(t *testing.T) {
	defer func(v, c, d string) { Version, Commit, Date = v, c, d }(Version, Commit, Date)
	Version, Commit, Date = "0.21.0", "abc1234", "2026-11-06T10:00:00Z"
	if got, want := String(), "0.21.0 (abc1234, 2026-11-06T10:00:00Z)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
