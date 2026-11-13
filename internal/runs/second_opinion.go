package runs

import (
	"context"
	"fmt"
	"strings"
)

// SecondOpinionResult is what a finished second opinion tells the Slack
// relay (and anyone else listening): the run, the runner and head it
// reviewed, and the section written into the item.
type SecondOpinionResult struct {
	RunID, ItemID, Runner, HeadSHA string
	Verdict                        string // pass | fail | "" (the run itself failed)
	Heading, Body                  string
}

// SetSecondOpinionNotifier wires the hook told about every finished second
// opinion (the Slack relay posts it to the item's thread).
func (s *Service) SetSecondOpinionNotifier(fn func(ctx context.Context, r SecondOpinionResult)) {
	s.secondOpinionNotify = fn
}

// QueueSecondOpinion queues one advisory review of headSHA on runner. The
// inbox decides who may ask and when; the one-active-run-per-stage rule
// refuses a second request while one is in flight.
func (s *Service) QueueSecondOpinion(ctx context.Context, itemID, workspaceID, runner, headSHA, branch, baseSHA string) (string, error) {
	r, err := s.Queue(ctx, QueueInput{
		WorkspaceID: workspaceID, ItemID: itemID, Stage: StageSecondOpinion,
		Runner: runner, HeadSHA: headSHA, Branch: branch, BaseSHA: baseSHA,
	})
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

// secondOpinionDone writes the result into the item, titled with the runner
// and the head from the run record (never from the agent), and notes when
// the item has moved on to a newer head since.
func (s *Service) secondOpinionDone(ctx context.Context, run *Run, in ReportInput) {
	heading := fmt.Sprintf("Second opinion — %s @ %s", run.Runner, short(run.HeadSHA))
	var b strings.Builder
	verdict := ""
	if run.Status == StatusSucceeded && (in.Verdict == "pass" || in.Verdict == "fail") {
		verdict = in.Verdict
		fmt.Fprintf(&b, "**Verdict:** %s (advisory: the item's status is unchanged)\n\n%s\n", verdict, strings.TrimSpace(in.Summary))
		var blocking, nits []Finding
		for _, f := range in.Findings {
			if f.Severity == SeverityBlocking {
				blocking = append(blocking, f)
			} else {
				nits = append(nits, f)
			}
		}
		writeFindings(&b, "Blocking", blocking)
		writeFindings(&b, "Nits", nits)
		// What it said about its own earlier findings: a verdict of fail
		// with nothing new blocking is explained here.
		b.WriteString(s.earlierAnswers(ctx, run))
	} else {
		fmt.Fprintf(&b, "The second opinion did not finish: %s\n", firstLine(in.Reason, in.Summary, "no reason given"))
	}
	if latest := s.latestHead(ctx, run.ItemID); latest != "" && latest != run.HeadSHA {
		fmt.Fprintf(&b, "\n_Reviewed an older head (%s); the item's head is now %s._\n", short(run.HeadSHA), short(latest))
	}
	if run.CostUSD != nil && *run.CostUSD > 0 {
		fmt.Fprintf(&b, "\n_Cost: $%.2f._\n", *run.CostUSD)
	}
	body := b.String()
	if s.itemAppend != nil {
		if err := s.itemAppend(ctx, run.ItemID, heading, body); err != nil {
			s.chainNote(ctx, run, "writing the second opinion failed: "+err.Error())
		}
	}
	if s.secondOpinionNotify != nil {
		s.secondOpinionNotify(ctx, SecondOpinionResult{RunID: run.ID, ItemID: run.ItemID, Runner: run.Runner,
			HeadSHA: run.HeadSHA, Verdict: verdict, Heading: heading, Body: body})
	}
}

func writeFindings(b *strings.Builder, title string, fs []Finding) {
	if len(fs) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### %s\n\n", title)
	for _, f := range fs {
		loc := ""
		if f.File != "" {
			loc = "`" + f.File + "` "
		}
		fmt.Fprintf(b, "- %s%s\n", loc, strings.TrimSpace(f.Text))
	}
}

// latestHead is the head the item is waiting on: its newest successful
// implement run's head (the same commit mark-done approves).
func (s *Service) latestHead(ctx context.Context, itemID string) string {
	runs, err := s.storage.List(ctx, "", itemID)
	if err != nil {
		return ""
	}
	for _, r := range runs {
		if (r.Stage == StageImplement || r.Stage == StageBranchReview) && r.Status == StatusSucceeded && r.HeadSHA != "" {
			return r.HeadSHA
		}
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
