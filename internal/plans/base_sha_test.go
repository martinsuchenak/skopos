package plans

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ValidBaseSHA: only a full object id is a base. A git option, a revision
// selector, a ref name, or an abbreviated id is not — each reaches the
// worker's git as an argument.
func TestValidBaseSHA(t *testing.T) {
	full40 := strings.Repeat("a", 40)
	full64 := strings.Repeat("b", 64)
	cases := []struct {
		base string
		ok   bool
	}{
		{"", true}, {full40, true}, {full64, true}, {" " + full40 + " ", true},
		{"--no-checkout", false}, {"--lock", false}, {"--output=/tmp/x", false},
		{"HEAD~1", false}, {"origin/main", false}, {"refs/heads/agent/x", false}, {"main", false},
		{"abc", false}, {"aaaa111", false}, {"0a1b2c3d4e5", false}, {strings.Repeat("a", 39), false}, {strings.Repeat("a", 41), false},
		{strings.Repeat("A", 40), false}, {strings.Repeat("g", 40), false}, {"-" + full40, false}, {full40 + "\n", true},
	}
	for _, c := range cases {
		if got := ValidBaseSHA(c.base); got != c.ok {
			t.Errorf("ValidBaseSHA(%q) = %v, want %v", c.base, got, c.ok)
		}
	}
}

// Every entry point a revision's base is written through refuses a base that
// is not a commit id: CreateRevision/AmendRevision (snapshot), Amend
// (AmendTx), Replan (ReplaceTx) and CreateCandidate.
func TestRevisionWritersRefuseANonCommitBase(t *testing.T) {
	svc, _, planID, g1 := candidateFixture(t)
	ctx := context.Background()
	steps := []RevisionStep{{Title: "s"}}
	if _, err := svc.AddItem(ctx, planID, CreateItemInput{Title: "s"}); err != nil {
		t.Fatalf("add a step: %v", err)
	}
	bad := []string{"--no-checkout", "--lock", "HEAD~1", "origin/main", "abc"}
	// A first revision with a full id, so AmendRevision has a latest to
	// amend over (it reads the latest before touching the base).
	if _, err := svc.CreateRevision(ctx, planID, strings.Repeat("c", 40)); err != nil {
		t.Fatalf("CreateRevision(full id): %v", err)
	}

	for _, b := range bad {
		if _, err := svc.CreateRevision(ctx, planID, b); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("CreateRevision(%q): want ErrInvalidInput, got %v", b, err)
		}
		if _, err := svc.AmendRevision(ctx, planID, b); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("AmendRevision(%q): want ErrInvalidInput, got %v", b, err)
		}
		if _, err := svc.Replan(ctx, planID, steps, b); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("Replan(%q): want ErrInvalidInput, got %v", b, err)
		}
		if _, err := svc.CreateCandidate(ctx, planID, steps, b, nil, g1, "claude"); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("CreateCandidate(%q): want ErrInvalidInput, got %v", b, err)
		}
	}

	// Nothing was written under a refused base.
	if revs, err := svc.ListRevisions(ctx, planID); err != nil {
		t.Fatal(err)
	} else {
		for _, r := range revs {
			if !ValidBaseSHA(r.BaseSHA) {
				t.Fatalf("a non-commit base was stored: %+v", r)
			}
		}
	}

	// A full commit id is accepted and stored as given.
	full := strings.Repeat("c", 40)
	rev, err := svc.CreateRevision(ctx, planID, full)
	if err != nil {
		t.Fatalf("CreateRevision(full id): %v", err)
	}
	if rev.BaseSHA != full {
		t.Fatalf("base not stored: %q", rev.BaseSHA)
	}
}
