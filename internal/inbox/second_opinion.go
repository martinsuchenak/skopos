package inbox

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// Second opinion (tmp/second-opinion-review — design.md): one extra,
// advisory review of the exact head an item at in_review is waiting on, by
// a runner Martin picks. It never moves or blocks the item; the run queue
// appends the result as a section.

// SecondOpinionQueuer is the run-queue hook (runs.Service satisfies it).
type SecondOpinionQueuer interface {
	QueueSecondOpinion(ctx context.Context, itemID, workspaceID, runner, headSHA, branch, baseSHA string) (string, error)
}

// ErrSecondOpinionConflict: one is already running for the item, or the
// runner already reviewed this head (pass force to run it again).
var ErrSecondOpinionConflict = errors.New("second opinion conflict")

// ErrPolicyRefused: the workspace's runner policy does not allow the runner
// for the stage (answered 403 with reason "policy").
var ErrPolicyRefused = errors.New("refused by the runner policy")

var reRunnerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// SecondOpinionInput: Runner empty = the workspace's default
// second-opinion runner (pipeline settings).
type SecondOpinionInput struct {
	Runner string `json:"runner,omitempty"`
	Force  bool   `json:"force,omitempty"`
	Via    string `json:"via,omitempty"`
}

// SecondOpinionRequest is what was queued.
type SecondOpinionRequest struct {
	RunID   string `json:"run_id"`
	Runner  string `json:"runner"`
	HeadSHA string `json:"head_sha"`
}

// RunnerPolicy answers whether a named runner may work on a workspace's
// stage under its policy: known is false when no worker serving the
// workspace reports that runner (nothing can be said yet — the claim still
// decides); refused names the policy that would not allow it.
type RunnerPolicy func(ctx context.Context, workspaceID, stage, runner string) (allowed, known bool, refused string, err error)

// SetRunnerPolicy wires the policy lookup (pipeline settings).
func (s *Service) SetRunnerPolicy(fn RunnerPolicy) { s.runnerPolicy = fn }

// SetSecondOpinionDefault wires the default-runner lookup (pipeline
// settings: the workspace's, else its group's).
func (s *Service) SetSecondOpinionDefault(fn func(ctx context.Context, workspaceID string) string) {
	s.secondOpinionDefault = fn
}

// RequestSecondOpinion queues a second opinion. Approver-only, like every
// human workflow action; audited as inbox.second_opinion.
func (s *Service) RequestSecondOpinion(ctx context.Context, itemID string, in SecondOpinionInput) (*SecondOpinionRequest, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	item, err := s.store.GetItem(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	if err := auth.RequireApprover(ctx); err != nil {
		return nil, err
	}
	queuer, ok := s.runQueuer.(SecondOpinionQueuer)
	if !ok || s.runLister == nil {
		return nil, fmt.Errorf("%w: the run queue is not available", ErrInvalidInput)
	}
	itemRuns, err := s.runLister.ListRuns(ctx, item.ID) // newest first
	if err != nil {
		return nil, err
	}
	if err := secondOpinionAllowed(item, itemRuns); err != nil {
		return nil, err
	}
	runner := strings.TrimSpace(in.Runner)
	if runner == "" && s.secondOpinionDefault != nil {
		runner = s.secondOpinionDefault(ctx, item.WorkspaceID)
	}
	if runner == "" {
		return nil, fmt.Errorf("%w: no default second-opinion runner for %s — name one", ErrInvalidInput, item.WorkspaceID)
	}
	if !reRunnerName.MatchString(runner) {
		return nil, fmt.Errorf("%w: runner %q: letters, digits, '.', '_' or '-'", ErrInvalidInput, runner)
	}
	// A runner the workspace's policy refuses is turned down here, not
	// queued to wait for a worker that may never be allowed to take it.
	if s.runnerPolicy != nil {
		allowed, known, refused, err := s.runnerPolicy(ctx, item.WorkspaceID, "second_opinion", runner)
		if err != nil {
			return nil, err
		}
		if known && !allowed {
			return nil, fmt.Errorf("%w: the %s policy does not allow runner %s for second opinions on %s", ErrPolicyRefused, refused, runner, item.WorkspaceID)
		}
	}
	head, branch, err := SecondOpinionHead(item, itemRuns)
	if err != nil {
		return nil, err
	}
	for _, r := range itemRuns {
		if r.Stage == "second_opinion" && r.Status == "succeeded" && r.Runner == runner && r.HeadSHA == head && !in.Force {
			return nil, fmt.Errorf("%w: %s already reviewed head %s — pass force to run it again", ErrSecondOpinionConflict, runner, shortSHA(head))
		}
	}
	_, baseSHA := s.approvedRevision(ctx, item)
	runID, err := queuer.QueueSecondOpinion(ctx, item.ID, item.WorkspaceID, runner, head, branch, baseSHA)
	if errors.Is(err, runs.ErrDuplicateActive) { // a concurrent request won the race
		return nil, fmt.Errorf("%w: a second opinion is already running for this item", ErrSecondOpinionConflict)
	}
	if err != nil {
		return nil, err
	}
	notes := fmt.Sprintf("second opinion: %s on head %s", runner, shortSHA(head))
	if in.Force {
		notes += " (forced)"
	}
	s.auditItem(ctx, item.ID, "inbox.second_opinion", in.Via, notes, "")
	return &SecondOpinionRequest{RunID: runID, Runner: runner, HeadSHA: head}, nil
}

// SecondOpinionHead is the head a second opinion would review now, or why
// none can be requested: the item must allow one (secondOpinionAllowed),
// have a head mark-done would approve — the newest successful implement
// run, or on a review-only item the reviewed head — and have no second
// opinion running. itemRuns are newest first. The state endpoint offers
// the action by the same rule.
func SecondOpinionHead(item *Item, itemRuns []RunSummary) (head, branch string, err error) {
	if err := secondOpinionAllowed(item, itemRuns); err != nil {
		return "", "", err
	}
	for _, r := range itemRuns {
		if (r.Stage == "implement" || r.Stage == StageBranchReview) && r.Status == "succeeded" && r.HeadSHA != "" {
			head, branch = r.HeadSHA, r.Branch
			break
		}
	}
	if head == "" {
		return "", "", fmt.Errorf("%w: the item has no implemented head to review", ErrInvalidInput)
	}
	for _, r := range itemRuns {
		if r.Stage != "second_opinion" {
			continue
		}
		switch r.Status {
		case "queued", "claimed", "running", "paused", "interrupted":
			return "", "", fmt.Errorf("%w: a second opinion (%s) is already running for this item", ErrSecondOpinionConflict, r.Runner)
		}
	}
	return head, branch, nil
}

// secondOpinionAllowed: at in_review, or blocked by its review — a review
// or QA run since the latest implementation failed (with a review group, the
// newest member may have passed while another one blocked). Not before the
// review: the regular review has not happened yet.
func secondOpinionAllowed(item *Item, runs []RunSummary) error {
	switch item.Status {
	case StatusInReview:
		return nil
	case StatusBlocked:
		for _, r := range runs { // newest first, back to the latest implementation
			if r.Stage == "implement" {
				break
			}
			if (r.Stage == "review" || r.Stage == "qa") && r.Status == "failed" {
				return nil
			}
		}
		return fmt.Errorf("%w: the item is blocked, but not by its review (a second opinion reviews a finished branch)", ErrInvalidInput)
	}
	return fmt.Errorf("%w: a second opinion applies to items at in_review or blocked by their review (item is %s)", ErrInvalidInput, statusLabel(item.Status))
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
