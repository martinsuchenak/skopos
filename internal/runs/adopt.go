package runs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// HeadMoved is what a review or QA run reports when the branch holds
// commits after the one it was given: someone (a developer) committed to
// it between the stages, pushed to origin or, with a branch checkout, in
// the worker's clone (Local: the commits are only there).
type HeadMoved struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Local bool   `json:"local,omitempty"`
}

// AdoptedCommit is one commit an adopt run took in: what a person added to
// the branch after the implementation (Observed.Adopted).
type AdoptedCommit struct {
	SHA     string `json:"sha"`
	Author  string `json:"author,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// maxAdoptRounds: how often the same implemented commit may be taken past
// before the item waits for a person. Adopting again from the same commit
// means the new commits never reached the worker that checks them (they
// are in another worker's clone and were never pushed).
const maxAdoptRounds = 2

// adoptHead takes in the commits a review or QA run found after the
// implemented one: the round in flight is cancelled and an implement run
// that adopts them is queued (setup and the checks on the branch as it is,
// a fix round when a check fails, the push), after which review and QA run
// again on the new head. Commits only in one worker's clone wait for that
// worker.
func (s *Service) adoptHead(ctx context.Context, run *Run, m HeadMoved) {
	if s.itemClosed != nil && s.itemClosed(ctx, run.ItemID) {
		return
	}
	from := strings.TrimSpace(m.From)
	if from == "" {
		from = run.HeadSHA
	}
	existing, err := s.storage.List(ctx, "", run.ItemID)
	if err != nil {
		s.chainNote(ctx, run, "reading the item's runs to take in the new commits failed: "+err.Error())
		return
	}
	// A run already taking in commits after this one (a panel sibling found
	// the same new commits first) is left alone: cancelling it here would
	// cost it a round. A cancelled one never ran its checks and is not a
	// round.
	rounds := 0
	for _, r := range existing {
		if r.Stage != StageImplement || r.AdoptFrom != from {
			continue
		}
		switch r.Status {
		case StatusQueued, StatusClaimed, StatusRunning, StatusPaused:
			return
		case StatusCancelled:
		default:
			rounds++
		}
	}
	agent := "worker:" + run.WorkerID
	if rounds >= maxAdoptRounds {
		where := "origin"
		if m.Local {
			where = run.WorkerID + "'s clone"
		}
		reason := fmt.Sprintf("the branch has commits after %s (at %s in %s) that could not be checked after %d tries: push them to origin, then Retry", short(from), short(m.To), where, rounds)
		if err := s.itemTransition(ctx, run.ItemID, "blocked", reason, agent); err != nil {
			slog.Warn("blocking an item whose new commits could not be checked", "item", run.ItemID, "err", err)
		}
		return
	}
	// The round in flight tested (or would test) the older commit: it ends
	// here, panel siblings included, so nothing decides on it.
	_ = s.CancelItem(ctx, run.ItemID, "the branch moved on to "+short(m.To)+": checking the new commits first")
	in := QueueInput{
		WorkspaceID: run.WorkspaceID, ItemID: run.ItemID, PlanRevID: run.PlanRevID, Stage: StageImplement,
		Branch: run.Branch, BaseSHA: run.BaseSHA, AdoptFrom: from,
	}
	if m.Local {
		in.PreferWorker = run.WorkerID
	}
	if _, err := s.Queue(ctx, in); err != nil {
		s.chainNote(ctx, run, "queuing the check of the new commits failed: "+err.Error())
		if err := s.itemTransition(ctx, run.ItemID, "blocked", "the branch moved on to "+short(m.To)+" and checking it could not be queued: "+err.Error(), agent); err != nil {
			slog.Warn("blocking an item after a failed adopt queue", "item", run.ItemID, "err", err)
		}
		return
	}
	s.chainNote(ctx, run, "the branch moved on from "+short(from)+" to "+short(m.To)+"; checking the new commits before "+run.Stage)
}

// QueueAdopt queues an implement run that takes in the commits on the
// branch after from (the approver's "Check the new commits" at sign-off).
// prefer: the worker whose clone holds them, "" for any.
func (s *Service) QueueAdopt(ctx context.Context, itemID, workspaceID, planRevID, baseSHA, branch, from, prefer string) (string, error) {
	r, err := s.Queue(ctx, QueueInput{
		WorkspaceID: workspaceID, ItemID: itemID, Stage: StageImplement, PlanRevID: planRevID,
		BaseSHA: baseSHA, Branch: branch, AdoptFrom: from, PreferWorker: prefer,
	})
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

// ImplementedHead is the head of the item's newest successful
// implementation: what review and QA test and a sign-off approves.
func (s *Service) ImplementedHead(ctx context.Context, itemID string) (string, error) {
	existing, err := s.storage.List(ctx, "", itemID)
	if err != nil {
		return "", err
	}
	return implementedHead(existing), nil
}
