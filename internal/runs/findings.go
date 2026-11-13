package runs

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/ids"
)

// SetFindings wires the record of findings (internal/findings): a run's
// report writes into it, and a run's prompt reads from it.
func (s *Service) SetFindings(fs *findings.Service) {
	s.findings = fs
	s.storage.findings = fs
}

func findingRun(r *Run) findings.Run {
	return findings.Run{ID: r.ID, WorkspaceID: r.WorkspaceID, ItemID: r.ItemID, Stage: r.Stage,
		Runner: r.Runner, Focus: r.Focus, HeadSHA: r.HeadSHA}
}

func newFindings(fs []Finding) []findings.NewFinding {
	out := make([]findings.NewFinding, 0, len(fs))
	for _, f := range fs {
		out = append(out, findings.NewFinding{ID: f.ID, Severity: f.Severity, Text: f.Text, File: f.File})
	}
	return out
}

// nameFindings gives each reported finding its id in the record. The
// server names them: an id a worker sent is never kept.
func nameFindings(fs []Finding) {
	for i := range fs {
		fs[i].ID = ids.New()
	}
}

// ReadFindings is the part of the item's findings the run acts on, for the
// worker holding the run (its key and lease): what an implement run is to
// fix, what a review is to check again, what other reviewers check, and
// what the approver accepted.
func (s *Service) ReadFindings(ctx context.Context, runID, workerID, leaseToken string) (*findings.View, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || strings.TrimSpace(workerID) == "" || leaseToken == "" {
		return nil, fmt.Errorf("%w: run id, worker and lease are required", ErrInvalidInput)
	}
	if s.findings == nil {
		return nil, fmt.Errorf("%w: the findings record is not configured", ErrInvalidInput)
	}
	if err := s.checkScope(ctx, runID); err != nil {
		return nil, err
	}
	if _, err := s.storage.ClaimHolder(ctx, runID, workerID, callerKeyID(ctx), hashLeaseToken(leaseToken)); err != nil {
		return nil, err
	}
	r, err := s.storage.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	round, err := findingRound(ctx, s.storage.db, r)
	if err != nil {
		return nil, err
	}
	return s.findings.ForRun(ctx, findingRun(r), round)
}

// findingRound is the reviewers a review run runs alongside: its panel's
// members in the order they were queued (run ids sort by time; the record
// gives each finding to one of them), or the run alone. Read through the
// report's transaction when a report records, on its own for a prompt.
func findingRound(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, r *Run) (findings.Round, error) {
	if r.GroupID == "" {
		return findings.Round{}, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT id, stage, COALESCE(focus, '') FROM runs WHERE group_id = ? ORDER BY id ASC`, r.GroupID)
	if err != nil {
		return findings.Round{}, fmt.Errorf("listing the round's reviewers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	round := findings.Round{}
	for rows.Next() {
		var m findings.Member
		if err := rows.Scan(&m.RunID, &m.Stage, &m.Focus); err != nil {
			return findings.Round{}, err
		}
		round.Members = append(round.Members, m)
	}
	return round, rows.Err()
}

// earlierAnswers renders what a review said about earlier findings, for
// the section its result is shown in: the ones it found still open, then
// the ones it found addressed. "" when it answered none.
func (s *Service) earlierAnswers(ctx context.Context, run *Run) string {
	if s.findings == nil {
		return ""
	}
	answers, err := s.findings.RunAnswers(ctx, run.ItemID, run.ID)
	if err != nil || len(answers) == 0 {
		return ""
	}
	var b strings.Builder
	for _, group := range []struct{ kind, title string }{{findings.KindStillOpen, "Earlier findings still open"}, {findings.KindAddressed, "Earlier findings addressed"}} {
		n := 0
		for _, a := range answers {
			if a.Kind != group.kind {
				continue
			}
			if n == 0 {
				fmt.Fprintf(&b, "\n**%s:**\n", group.title)
			}
			n++
			note := ""
			if a.Note != "" && a.Note != findings.NotRaisedAgain {
				note = " — " + a.Note
			}
			fmt.Fprintf(&b, "- (%s, raised by %s) %s%s\n", a.Finding.Severity, a.Finding.By(), a.Finding.Text, note)
		}
	}
	return b.String()
}
