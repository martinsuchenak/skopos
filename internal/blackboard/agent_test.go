package blackboard

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// An agent's read is one copy, bounded: the branch's own entries first,
// then the newest, each with its id, and a closing line naming what was
// left out.
func TestBundleForAgent(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var entries []Entry
	for i := range 200 {
		entries = append(entries, Entry{
			ID: fmt.Sprintf("p-%03d", i), Scope: ScopeProject, EntryType: TypeDecision,
			Title: fmt.Sprintf("project decision %03d", i), Content: strings.Repeat("x", 400),
			AuthorAgentID: "a", UpdatedAt: base.Add(time.Duration(i) * time.Hour),
		})
	}
	entries = append(entries, Entry{ID: "b-1", Scope: ScopeBranch, BranchName: "feat", EntryType: TypeFinding,
		Title: "the branch's own", AuthorAgentID: "a", UpdatedAt: base},
		// A bug of another branch floats into every read, but is not this
		// branch's own: it ranks by age with the project's entries.
		Entry{ID: "o-1", Scope: ScopeBranch, BranchName: "other", EntryType: TypeBug,
			Title: "another branch's old bug", AuthorAgentID: "a", UpdatedAt: base.Add(-time.Hour)})
	b := &Bundle{Entries: entries}

	text, shown := b.ForAgent("feat", MaxAgentBytes)
	if len(text) > MaxAgentBytes {
		t.Fatalf("over the bound: %d bytes", len(text))
	}
	if shown <= 1 || shown >= len(entries) {
		t.Fatalf("shown = %d of %d", shown, len(entries))
	}
	for _, want := range []string{"the branch's own", "id b-1", "project decision 199", "Provenance",
		fmt.Sprintf("%d more entries are not shown", len(entries)-shown), "`q`"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(text, "project decision 000") || strings.Contains(text, "another branch's old bug") {
		t.Error("the oldest entries not the branch's own should be the first left out")
	}

	small := &Bundle{Entries: entries[195:]}
	text, shown = small.ForAgent("feat", MaxAgentBytes)
	if shown != len(small.Entries) || strings.Contains(text, "not shown") {
		t.Fatalf("a small bundle is shown whole: %d of %d", shown, len(small.Entries))
	}
	if text, shown := (&Bundle{}).ForAgent("", MaxAgentBytes); shown != 0 || !strings.Contains(text, "No entries") {
		t.Fatalf("empty: %q", text)
	}
}
