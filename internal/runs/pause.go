package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// A worker's pause: an operator stops it taking work (someone works on its
// machine by hand) without stopping the process. While paused it claims
// nothing — not even its own interrupted runs — and does no idle work
// (indexing, cleanup); a run it already holds finishes. The pause ends
// when someone resumes it, or at Until when one is set.

// WorkerPause is a pause in force.
type WorkerPause struct {
	At     time.Time `json:"at"`
	Until  time.Time `json:"until,omitzero"` // zero: until resumed
	By     string    `json:"by,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// activeAt: the pause still holds at now.
func (p *WorkerPause) activeAt(now time.Time) bool {
	return p != nil && (p.Until.IsZero() || now.Before(p.Until))
}

// pausedSQL is true for a workers row (alias w) paused at the bound time.
const pausedSQL = `(w.paused_at IS NOT NULL AND (w.paused_until IS NULL OR w.paused_until > ?))`

// PauseInput: For (a duration such as "2h") or Until bound the pause;
// neither means until resumed.
type PauseInput struct {
	For    string    `json:"for,omitempty"`
	Until  time.Time `json:"until,omitzero"`
	Reason string    `json:"reason,omitempty"`
	Via    string    `json:"-"`
}

// maxPauseReason bounds the stored reason.
const maxPauseReason = 500

// AuditRecorder writes the pause and resume entries (internal/audit).
type AuditRecorder interface {
	Record(ctx context.Context, in audit.RecordInput) error
}

// SetAuditRecorder wires the audit log (nil = no entries).
func (s *Service) SetAuditRecorder(a AuditRecorder) { s.audit = a }

// PauseWorker pauses a worker (see mayManageWorker for who may).
func (s *Service) PauseWorker(ctx context.Context, workerID string, in PauseInput) (*WorkerPause, error) {
	workerID = strings.TrimSpace(workerID)
	if err := s.mayManageWorker(ctx, workerID); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	p := &WorkerPause{At: now, By: actorName(ctx), Reason: strings.TrimSpace(in.Reason)}
	if len(p.Reason) > maxPauseReason {
		p.Reason = p.Reason[:maxPauseReason]
	}
	switch {
	case in.For != "" && !in.Until.IsZero():
		return nil, fmt.Errorf("%w: give for or until, not both", ErrInvalidInput)
	case in.For != "":
		d, err := time.ParseDuration(strings.TrimSpace(in.For))
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("%w: for must be a positive duration such as 90m or 2h", ErrInvalidInput)
		}
		p.Until = now.Add(d)
	case !in.Until.IsZero():
		if !in.Until.After(now) {
			return nil, fmt.Errorf("%w: until must be in the future", ErrInvalidInput)
		}
		p.Until = in.Until.UTC()
	}
	if err := s.storage.SetWorkerPause(ctx, workerID, p); err != nil {
		return nil, err
	}
	notes := "until resumed"
	if !p.Until.IsZero() {
		notes = "until " + p.Until.Format(time.RFC3339)
	}
	if p.Reason != "" {
		notes += ": " + p.Reason
	}
	s.recordAudit(ctx, workerID, "worker.pause", in.Via, notes)
	return p, nil
}

// ResumeWorker ends a worker's pause (a no-op when it has none).
func (s *Service) ResumeWorker(ctx context.Context, workerID, via string) error {
	workerID = strings.TrimSpace(workerID)
	if err := s.mayManageWorker(ctx, workerID); err != nil {
		return err
	}
	was, err := s.storage.WorkerPause(ctx, workerID)
	if err != nil {
		return err
	}
	if err := s.storage.SetWorkerPause(ctx, workerID, nil); err != nil {
		return err
	}
	if was.activeAt(s.now().UTC()) {
		s.recordAudit(ctx, workerID, "worker.resume", via, "")
	}
	return nil
}

// WorkerPaused is the worker's pause in force, or nil (the claim answer
// tells the worker so it skips its idle work).
func (s *Service) WorkerPaused(ctx context.Context, workerID string) *WorkerPause {
	p, err := s.storage.WorkerPause(ctx, strings.TrimSpace(workerID))
	if err != nil || !p.activeAt(s.now().UTC()) {
		return nil
	}
	return p
}

// mayManageWorker: root, the key the worker runs with (its own CLI on its
// host), or an approver whose scope covers every workspace the worker
// serves — a pause stops work for all of them. Others don't see the
// worker (404) or lack the approver permission (403).
func (s *Service) mayManageWorker(ctx context.Context, workerID string) error {
	if workerID == "" {
		return fmt.Errorf("%w: worker id is required", ErrInvalidInput)
	}
	keyID, workspaces, err := s.storage.workerOwner(ctx, workerID)
	if err != nil {
		return err
	}
	p := auth.PrincipalFromContext(ctx)
	if p.IsRoot() || (keyID.Valid && keyID.String == p.KeyID) {
		return nil
	}
	visible := false
	for _, ws := range workspaces {
		if p.CanAccess(ws) {
			visible = true
		}
	}
	if !visible {
		return fmt.Errorf("%w: worker %s", ErrNotFound, workerID)
	}
	if err := auth.RequireApprover(ctx); err != nil {
		return err
	}
	for _, ws := range workspaces {
		if !p.CanAccess(ws) {
			return fmt.Errorf("%w: the worker also serves workspaces outside your scope", auth.ErrApproverRequired)
		}
	}
	return nil
}

func (s *Service) recordAudit(ctx context.Context, workerID, action, via, notes string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Record(ctx, audit.RecordInput{EntityType: "worker", EntityID: workerID, Action: action, Via: via, Notes: notes})
}

// actorName names the caller the way the audit log does.
func actorName(ctx context.Context) string {
	actor, _ := auth.Actor(ctx)
	return actor
}

// workerOwner reads the key a worker's row belongs to and the workspaces
// it serves (ErrNotFound when the worker never polled).
func (s *Storage) workerOwner(ctx context.Context, workerID string) (sql.NullString, []string, error) {
	var keyID sql.NullString
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT key_id, workspaces FROM workers WHERE id = ?`, workerID).Scan(&keyID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return keyID, nil, fmt.Errorf("%w: worker %s", ErrNotFound, workerID)
	}
	if err != nil {
		return keyID, nil, err
	}
	var ws []string
	_ = json.Unmarshal([]byte(raw), &ws)
	return keyID, ws, nil
}

// SetWorkerPause stores a pause (nil clears it).
func (s *Storage) SetWorkerPause(ctx context.Context, workerID string, p *WorkerPause) error {
	var at, until, by, reason any
	if p != nil {
		at, by, reason = formatTime(p.At), p.By, p.Reason
		if !p.Until.IsZero() {
			until = formatTime(p.Until)
		}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE workers SET paused_at = ?, paused_until = ?, paused_by = ?, pause_reason = ? WHERE id = ?`,
		at, until, by, reason, workerID)
	if err != nil {
		return fmt.Errorf("storing the worker's pause: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: worker %s", ErrNotFound, workerID)
	}
	return nil
}

// WorkerPause reads the stored pause (nil when none; it may have ended).
func (s *Storage) WorkerPause(ctx context.Context, workerID string) (*WorkerPause, error) {
	var at, until, by, reason sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT paused_at, paused_until, paused_by, pause_reason FROM workers WHERE id = ?`, workerID).
		Scan(&at, &until, &by, &reason)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !at.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := &WorkerPause{At: parseTime(at.String), By: by.String, Reason: reason.String}
	if until.Valid {
		p.Until = parseTime(until.String)
	}
	return p, nil
}

// PauseWorker handles POST /api/workers/{id}/pause ({for, until, reason};
// an empty body pauses until resumed).
func (h *Handler) PauseWorker(w http.ResponseWriter, r *http.Request) {
	var in PauseInput
	if r.ContentLength != 0 {
		if err := rest.DecodeJSON(w, r, &in); err != nil {
			rest.RespondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	in.Via = rest.Via(r)
	p, err := h.service.PauseWorker(r.Context(), r.PathValue("id"), in)
	if err != nil {
		h.respondRunError(w, err, "pause worker")
		return
	}
	rest.RespondJSON(w, http.StatusOK, p)
}

// ResumeWorker handles POST /api/workers/{id}/resume.
func (h *Handler) ResumeWorker(w http.ResponseWriter, r *http.Request) {
	if err := h.service.ResumeWorker(r.Context(), r.PathValue("id"), rest.Via(r)); err != nil {
		h.respondRunError(w, err, "resume worker")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
