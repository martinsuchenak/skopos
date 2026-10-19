package inbox

import (
	"errors"
	"testing"
)

// When a second opinion applies: at in_review; when blocked by a review or
// QA run since the latest implementation (a failed member of a group
// counts even when a newer member passed); never when blocked by the
// implementation itself or before the review.
func TestSecondOpinionAllowed(t *testing.T) {
	impl := RunSummary{Stage: "implement", Status: "succeeded", HeadSHA: "abc"}
	cases := []struct {
		name   string
		status Status
		runs   []RunSummary // newest first
		ok     bool
	}{
		{"in review", StatusInReview, []RunSummary{{Stage: "review", Status: "succeeded"}, impl}, true},
		{"blocked by review", StatusBlocked, []RunSummary{{Stage: "review", Status: "failed"}, impl}, true},
		{"blocked by a group member", StatusBlocked, []RunSummary{{Stage: "review", Status: "succeeded"}, {Stage: "review", Status: "failed"}, impl}, true},
		{"blocked by qa", StatusBlocked, []RunSummary{{Stage: "qa", Status: "failed"}, {Stage: "review", Status: "succeeded"}, impl}, true},
		{"blocked by implementation", StatusBlocked, []RunSummary{{Stage: "implement", Status: "failed"}, {Stage: "review", Status: "failed"}, impl}, false},
		{"blocked before review", StatusBlocked, []RunSummary{impl}, false},
		{"implementing", StatusImplementing, []RunSummary{impl}, false},
		{"awaiting approval", StatusAwaitingApproval, nil, false},
	}
	for _, c := range cases {
		err := secondOpinionAllowed(&Item{Status: c.status}, c.runs)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalidInput)) {
			t.Errorf("%s: ok=%v err=%v", c.name, c.ok, err)
		}
	}
}
