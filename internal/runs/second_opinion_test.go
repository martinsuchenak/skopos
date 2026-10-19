package runs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A second opinion needs a runner and a head; it never moves the item; its
// section names the head from the run record and says when the item has
// moved on to a newer head; the notifier hears about it.
func TestSecondOpinionRun(t *testing.T) {
	s := testService(t)
	insertItem(t, s, "item-1")
	var moves, sections []string
	var heard []SecondOpinionResult
	s.SetItemTransition(func(_ context.Context, _, to, _, _ string) error { moves = append(moves, to); return nil })
	s.SetItemAppend(func(_ context.Context, _, heading, body string) error {
		sections = append(sections, heading+"\n"+body)
		return nil
	})
	s.SetSecondOpinionNotifier(func(_ context.Context, r SecondOpinionResult) { heard = append(heard, r) })
	ctx := rootCtx()

	if _, err := s.QueueSecondOpinion(ctx, "item-1", "ws-a", "", "abc1234def", "agent/x", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("no runner: %v", err)
	}
	if _, err := s.QueueSecondOpinion(ctx, "item-1", "ws-a", "codex", "", "agent/x", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("no head: %v", err)
	}
	// The item has two implementations; the second opinion reviews the older head.
	for _, head := range []string{"abc1234def", "fff9999aaa"} {
		r := mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
		c, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
		if c == nil || c.ID != r.ID {
			t.Fatalf("claim implement: %v", c)
		}
		if _, err := s.Report(ctx, c.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, HeadSHA: head, Branch: "agent/x"}, c.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	moves = nil
	id, err := s.QueueSecondOpinion(ctx, "item-1", "ws-a", "codex", "abc1234def", "agent/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueSecondOpinion(ctx, "item-1", "ws-a", "glm", "abc1234def", "agent/x", ""); !errors.Is(err, ErrDuplicateActive) {
		t.Fatalf("one at a time: %v", err)
	}
	if c, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Runners: []string{"claude"}, Workspaces: []string{"ws-a"}}); c != nil {
		t.Fatalf("pinned to codex, got %+v", c)
	}
	c, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Runners: []string{"codex"}, Workspaces: []string{"ws-a"}})
	if c == nil || c.ID != id || c.HeadSHA != "abc1234def" || c.Stage != StageSecondOpinion {
		t.Fatalf("claim second opinion: %+v", c)
	}
	if _, err := s.Report(ctx, c.ID, ReportInput{WorkerID: "w2", Status: StatusSucceeded, Verdict: "fail", Summary: "unsafe",
		Findings: []Finding{{Severity: "blocking", Text: "SQL by concatenation", File: "db.go:3"}}}, c.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if len(moves) != 0 {
		t.Fatalf("a second opinion never moves the item: %v", moves)
	}
	if len(sections) != 1 || !strings.HasPrefix(sections[0], "Second opinion — codex @ abc1234\n**Verdict:** fail (advisory") ||
		!strings.Contains(sections[0], "### Blocking") || !strings.Contains(sections[0], "`db.go:3` SQL by concatenation") ||
		!strings.Contains(sections[0], "Reviewed an older head (abc1234); the item's head is now fff9999") {
		t.Fatalf("section: %q", sections)
	}
	if got, _ := s.Get(ctx, c.ID); got.HeadSHA != "abc1234def" {
		t.Fatalf("the report must keep the reviewed head: %q", got.HeadSHA)
	}
	if len(heard) != 1 || heard[0].Verdict != "fail" || heard[0].Runner != "codex" || heard[0].HeadSHA != "abc1234def" {
		t.Fatalf("notifier: %+v", heard)
	}
}

// A second opinion parked by a quota limit stays parked when an amendment's
// approval resumes the item's runs: it comes back at its own reset.
func TestSecondOpinionNotResumedWithItem(t *testing.T) {
	s := testService(t)
	insertItem(t, s, "item-1")
	ctx := rootCtx()
	id, err := s.QueueSecondOpinion(ctx, "item-1", "ws-a", "codex", "abc1234def", "agent/x", "")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w2", Runners: []string{"codex"}, Workspaces: []string{"ws-a"}})
	if c == nil || c.ID != id {
		t.Fatalf("claim: %+v", c)
	}
	if err := s.Pause(ctx, c.ID, "w2", "quota", time.Now().Add(time.Hour), c.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeItem(ctx, "item-1"); err != nil {
		t.Fatal(err)
	}
	paused, err := s.storage.List(ctx, StatusPaused, "item-1")
	if err != nil || len(paused) != 1 || paused[0].ID != id {
		t.Fatalf("the second opinion must stay paused: %+v %v", paused, err)
	}
}
