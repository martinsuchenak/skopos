package runs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/martinsuchenak/skopos/internal/labels"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// Worker labels: what an operator says a worker has that others serving the
// same workspace may not — a client's data set, a licensed tool, a device.
// An item names the labels it needs (inbox_items.needs) and the claim hands
// its runs only to a worker with all of them (storage.go Claim, every
// scan). The labels live on the server, never in the worker's file: the
// worker reports nothing about them and no poll writes them.

// NeedsStageKeys are the stages a workspace may bind an item's needed
// labels to (pipeline setting needs_stages). Every run stage belongs to one
// of them: NeedsStageKey.
var NeedsStageKeys = []string{StagePlan, StageImplement, StageReview, StageQA}

// NeedsStageKey is the key a run's stage counts under. The advisory stages
// of a plan round go with the plan and a second opinion with the review. A
// review of an existing branch runs the repository's setup and checks
// before it reviews, so it goes with the implementation: it needs what the
// implementer needs of its worker.
func NeedsStageKey(stage string) string {
	switch stage {
	case StageSynthesize, StageCompare:
		return StagePlan
	case StageSecondOpinion:
		return StageReview
	case StageBranchReview:
		return StageImplement
	}
	return stage
}

// NeedsBind reports whether an item's needed labels bind a run of the
// stage. Without a setting (and without prefs: nothing known must never
// read as "no requirement") they bind every stage.
func (p *WorkspacePrefs) NeedsBind(stage string) bool {
	if p == nil || len(p.NeedsStages) == 0 {
		return true
	}
	return slices.Contains(p.NeedsStages, NeedsStageKey(stage))
}

// boundNeeds is what a run requires of the worker that takes it: its
// item's needed labels in a stage they bind, nothing in the others. Every
// place that matches a run against a worker's labels reads it through here.
func boundNeeds(r *Run, p *WorkspacePrefs) []string {
	if len(r.Needs) == 0 || !p.NeedsBind(r.Stage) {
		return nil
	}
	return r.Needs
}

// LabelsInput is the body of PUT /api/workers/{id}/labels: the full list
// (an empty one clears it).
type LabelsInput struct {
	Labels []string `json:"labels"`
	Via    string   `json:"-"`
}

// SetWorkerLabels replaces a worker's labels: root, or an approver whose
// scope covers every workspace the worker serves. The worker's own key may
// not. The worker must have polled once (ErrNotFound otherwise).
func (s *Service) SetWorkerLabels(ctx context.Context, workerID string, in LabelsInput) ([]string, error) {
	workerID = strings.TrimSpace(workerID)
	if err := s.mayActOnWorker(ctx, workerID, false); err != nil {
		return nil, err
	}
	list, err := labels.Normalize(in.Labels)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidInput, strings.TrimPrefix(err.Error(), labels.ErrInvalid.Error()+": "))
	}
	was, err := s.storage.SetWorkerLabels(ctx, workerID, list)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(was, list) {
		notes := "none"
		if len(list) > 0 {
			notes = strings.Join(list, ", ")
		}
		s.recordAudit(ctx, workerID, "worker.labels", in.Via, notes)
	}
	if list == nil {
		list = []string{}
	}
	return list, nil
}

// SetWorkerLabels stores a worker's labels and returns what it held before.
func (s *Storage) SetWorkerLabels(ctx context.Context, workerID string, list []string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var was string
	err = tx.QueryRowContext(ctx, `SELECT labels FROM workers WHERE id = ?`, workerID).Scan(&was)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: worker %s", ErrNotFound, workerID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the worker's labels: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workers SET labels = ? WHERE id = ?`, labels.JSON(list), workerID); err != nil {
		return nil, fmt.Errorf("storing the worker's labels: %w", err)
	}
	return labels.Parse(was), tx.Commit()
}

// SetWorkerLabels handles PUT /api/workers/{id}/labels.
func (h *Handler) SetWorkerLabels(w http.ResponseWriter, r *http.Request) {
	var in LabelsInput
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	in.Via = rest.Via(r)
	list, err := h.service.SetWorkerLabels(r.Context(), r.PathValue("id"), in)
	if err != nil {
		h.respondRunError(w, err, "label worker")
		return
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"labels": list})
}
