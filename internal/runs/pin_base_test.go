package runs

import (
	"errors"
	"strings"
	"testing"
)

// PinGroupBase hands the pinned base to every member's git as an argument:
// a git option ("--no-checkout") or a revision selector ("HEAD~1") must be
// refused before it is pinned for the group.
func TestPinGroupBaseRefusesANonCommitBase(t *testing.T) {
	s := testService(t)
	// A review group of two members, as newGroupFixture configures it.
	s.SetPrefs(map[string]*WorkspacePrefs{"ws-a": {
		Stages:   map[string]string{"implement": "claude"},
		Groups:   map[string][]Member{"review": {{Runner: "claude", Focus: "security"}, {Runner: "glm", Focus: "tests"}}},
		Policies: map[string]StagePolicy{"review": {}},
	}})
	insertItem(t, s, "item-1")
	// A review-stage queue opens a group under the default prefs.
	run, err := s.Queue(rootCtx(), QueueInput{WorkspaceID: "ws-a", ItemID: "item-1", Stage: StageReview})
	if err != nil {
		t.Fatal(err)
	}
	if run.GroupID == "" {
		t.Fatalf("expected a grouped run, got %+v", run)
	}
	for _, b := range []string{"--no-checkout", "--lock", "HEAD~1", "origin/main", "abc"} {
		if _, err := s.PinGroupBase(rootCtx(), run.ID, b); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("PinGroupBase(%q): want ErrInvalidInput, got %v", b, err)
		}
	}
	// A full commit id is pinned and returned for the group.
	full := strings.Repeat("a", 40)
	got, err := s.PinGroupBase(rootCtx(), run.ID, full)
	if err != nil || got != full {
		t.Fatalf("PinGroupBase(full id) = %q, %v", got, err)
	}
	// An ungrouped run pins nothing and returns the trimmed base as given
	// (nothing is stored for it): the validation above still applies.
	insertItem(t, s, "item-2")
	solo, err := s.Queue(rootCtx(), QueueInput{WorkspaceID: "ws-a", ItemID: "item-2", Stage: StageImplement, Single: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinGroupBase(rootCtx(), solo.ID, "--no-checkout"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("PinGroupBase(ungrouped, --no-checkout): want ErrInvalidInput, got %v", err)
	}
	if got, err := s.PinGroupBase(rootCtx(), solo.ID, full); err != nil || got != full {
		t.Fatalf("PinGroupBase(ungrouped, full id) = %q, %v", got, err)
	}
}
