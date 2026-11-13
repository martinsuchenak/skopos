package runs

import (
	"encoding/json"
	"strings"
	"testing"
)

// An earlier blocking finding a review found still open counts against it
// like a finding of its own, everywhere the pass rule is read: the rule
// itself, a branch-review panel's verdicts, the panel's summary and the
// review.finished event.
func TestStillOpenCountsAsBlocking(t *testing.T) {
	clean := Run{ID: "a", Stage: StageBranchReview, Status: StatusSucceeded, Runner: "claude", Focus: "security"}
	nits := Run{ID: "b", Stage: StageBranchReview, Status: StatusSucceeded, Runner: "glm", Focus: "tests", Findings: []Finding{{Severity: SeverityNit, Text: "rename"}}}
	still := Run{ID: "c", Stage: StageBranchReview, Status: StatusSucceeded, Runner: "codex", Focus: "perf", StillOpen: 2, Findings: []Finding{{Severity: SeverityNit, Text: "spacing"}}}

	if BlockingFindings(clean) != 0 || BlockingFindings(nits) != 0 || BlockingFindings(still) != 2 {
		t.Fatalf("blocking: %d %d %d", BlockingFindings(clean), BlockingFindings(nits), BlockingFindings(still))
	}
	if !ReviewPassed(clean) || !ReviewPassed(nits) || ReviewPassed(still) {
		t.Fatal("a review with an earlier finding still open did not pass")
	}

	// A branch review succeeds whatever it found; the panel's policy reads
	// a member with something blocking as failing.
	g := &Group{Stage: StageBranchReview, Policy: PolicyAnyBlocks, Size: 3, Outcome: "fail"}
	got := verdicts(g, []Run{clean, nits, still})
	if got[0].Status != StatusSucceeded || got[1].Status != StatusSucceeded || got[2].Status != StatusFailed {
		t.Fatalf("verdicts: %s %s %s", got[0].Status, got[1].Status, got[2].Status)
	}
	// An ordinary review group is taken as reported.
	if out := verdicts(&Group{Stage: StageReview}, []Run{still}); out[0].Status != StatusSucceeded {
		t.Fatalf("a review group's members keep their status: %s", out[0].Status)
	}

	sum := groupSummary(g, []Run{clean, nits, still})
	if !strings.Contains(sum, "fail — 0 blocking, 1 nit — 2 earlier still open") {
		t.Fatalf("the summary says why the member failed:\n%s", sum)
	}
	// A member that failed on earlier findings alone has no finding of its
	// own: the line names them instead of repeating its failure reason.
	alone := Run{ID: "d", Stage: StageReview, Status: StatusFailed, Runner: "claude", StillOpen: 1, FailReason: "review failed: the race is still there"}
	sum = groupSummary(&Group{Stage: StageReview, Policy: PolicyAnyBlocks, Size: 1, Outcome: "fail"}, []Run{alone})
	if !strings.Contains(sum, "fail — 1 earlier still open") || strings.Contains(sum, "the race is still there") {
		t.Fatalf("a member failing on earlier findings alone:\n%s", sum)
	}

	// The event: blocking includes them, nits do not go negative.
	raw, _ := json.Marshal(finishedEvents(&still))
	for _, want := range []string{`"blocking":2`, `"nits":1`, `"still_open":2`, `"passed":false`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("review.finished lacks %s: %s", want, raw)
		}
	}
	if raw, _ := json.Marshal(finishedEvents(&nits)); strings.Contains(string(raw), "still_open") || !strings.Contains(string(raw), `"passed":true`) {
		t.Fatalf("nothing still open, nothing said: %s", raw)
	}
}
