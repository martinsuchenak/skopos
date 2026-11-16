package runs

import (
	"strings"
	"testing"
)

// A report's observed facts are stored with the run, bounded, and a later
// report without them (a heartbeat-less retry, an older worker) keeps them.
func TestReportRecordsObserved(t *testing.T) {
	s := testService(t)
	ctx := rootCtx()
	_ = mustQueue(t, s, ctx, "ws-a", "item-1", StageImplement)
	run, _ := s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	long := strings.Repeat("x", 2000)
	files := []ChangedFile{{Path: "db/migrations/002.sql", Status: "A", Added: 3}, {Path: long, Status: "M", Added: -5, Removed: 1}, {Path: ""}}
	for range 1200 {
		files = append(files, ChangedFile{Path: "gen/f.go", Status: "M", Added: 1})
	}
	obs := &Observed{Change: &Change{Files: files, Added: 1204, Removed: 1, Commits: 2}}
	if _, err := s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, Summary: "done", HeadSHA: "abc", Observed: obs}, run.LeaseToken); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetFull(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := got.Observed.Change
	if c == nil || len(c.Files) != maxObservedFiles || c.Truncated != len(files)-1-maxObservedFiles { // the empty path is no file
		t.Fatalf("change kept %d files, truncated %d", len(c.Files), c.Truncated)
	}
	if c.Files[0].Path != "db/migrations/002.sql" || len(c.Files[1].Path) != maxObservedPath || c.Files[1].Added != -1 || c.Added != 1204 || c.Commits != 2 {
		t.Fatalf("files bounded: %+v %+v", c.Files[0], c.Files[1])
	}

	// A plan run's facts.
	_ = mustQueue(t, s, ctx, "ws-a", "item-2", StagePlan)
	run, _ = s.ClaimNext(ctx, ClaimInput{WorkerID: "w1", Workspaces: []string{"ws-a"}})
	plan := &Observed{Plan: &PlanFacts{Size: "m", Steps: 3, Irreversible: []string{"drops the legacy table", " ", long}}}
	if _, err := s.Report(ctx, run.ID, ReportInput{WorkerID: "w1", Status: StatusSucceeded, Summary: "plan ready", Observed: plan}, run.LeaseToken); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetFull(ctx, run.ID)
	if p := got.Observed.Plan; p == nil || p.Size != "m" || p.Steps != 3 || len(p.Irreversible) != 2 || len(p.Irreversible[1]) != maxObservedText {
		t.Fatalf("plan facts: %+v", got.Observed.Plan)
	}
	// Lists and plain reads leave it out: runs are polled where nothing
	// shows it.
	if plain, _ := s.Get(ctx, run.ID); plain.Observed != nil {
		t.Fatal("Get leaves observed out")
	}
	if list, _ := s.List(ctx, "", "item-1"); len(list) != 1 || list[0].Observed != nil {
		t.Fatal("a list leaves observed out")
	}
	if (&Observed{}).clean() != nil || observedArg(nil) != nil {
		t.Fatal("nothing observed stores nothing")
	}
}
