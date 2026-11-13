package runs

import "github.com/martinsuchenak/skopos/internal/integrations"

// Run events for other systems (tmp/integrations — design.md §3.2),
// written in the claim and report transactions.

// SetEventEmitter records run events (internal/integrations; nil = none).
func (s *Service) SetEventEmitter(e *integrations.Emitter) { s.storage.events = e }

func runData(r *Run) map[string]any {
	d := map[string]any{"run_id": r.ID, "stage": r.Stage}
	if r.Runner != "" {
		d["runner"] = r.Runner
	}
	if r.GroupID != "" {
		d["group_id"] = r.GroupID
	}
	return d
}

func startedEvent(r *Run) integrations.Event {
	d := runData(r)
	d["attempt"] = r.Attempt
	return integrations.Event{Type: integrations.TypeRunStarted, ItemID: r.ItemID, WorkspaceID: r.WorkspaceID, Data: d}
}

func reviewStage(stage string) bool {
	return stage == StageReview || stage == StageBranchReview || stage == StageSecondOpinion
}

// finishedEvents: run.finished always; checks.finished and pr.opened for
// an implement run that ran checks or has a pull request; review.finished
// for a review-like run.
func finishedEvents(r *Run) []integrations.Event {
	ev := func(t string, d map[string]any) integrations.Event {
		return integrations.Event{Type: t, ItemID: r.ItemID, WorkspaceID: r.WorkspaceID, Data: d}
	}
	d := runData(r)
	d["outcome"] = string(r.Status)
	if r.FailReason != "" {
		d["reason"] = r.FailReason
	}
	if r.HeadSHA != "" {
		d["head_sha"] = r.HeadSHA
	}
	out := []integrations.Event{ev(integrations.TypeRunFinished, d)}
	if r.Stage == StageImplement && len(r.Checks) > 0 {
		failed := []string{}
		for _, c := range r.Checks {
			if !c.Passed {
				failed = append(failed, c.Name)
			}
		}
		out = append(out, ev(integrations.TypeChecksFinished, map[string]any{
			"run_id": r.ID, "head_sha": r.HeadSHA, "passed": len(failed) == 0, "failed_names": failed}))
	}
	if r.Stage == StageImplement && r.PRURL != "" {
		out = append(out, ev(integrations.TypePROpened, map[string]any{"run_id": r.ID, "url": r.PRURL, "head_sha": r.HeadSHA}))
	}
	if reviewStage(r.Stage) {
		rd := runData(r)
		blocking := BlockingFindings(*r)
		rd["head_sha"] = r.HeadSHA
		rd["passed"] = ReviewPassed(*r)
		rd["blocking"] = blocking
		rd["nits"] = len(r.Findings) - (blocking - r.StillOpen)
		if r.StillOpen > 0 {
			rd["still_open"] = r.StillOpen
		}
		if r.Focus != "" {
			rd["focus"] = r.Focus
		}
		out = append(out, ev(integrations.TypeReviewFinished, rd))
	}
	return out
}
