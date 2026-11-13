package inbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// FindingLog is the record of review findings (internal/findings): what
// each review found and what became of it.
type FindingLog interface {
	Item(ctx context.Context, itemID string) ([]findings.Finding, error)
	Check(ctx context.Context, itemID string, ids []string) error
	SendBack(ctx context.Context, itemID string, ids []string, who findings.Who) ([]findings.Finding, error)
	Waive(ctx context.Context, itemID, id, note string, who findings.Who) (*findings.Finding, error)
}

// SetFindingLog wires the record of review findings.
func (s *Service) SetFindingLog(f FindingLog) { s.findings = f }

// Findings is the item's record of review findings, oldest first, each
// with its state and what happened to it.
func (s *Service) Findings(ctx context.Context, itemID string) ([]findings.Finding, error) {
	fs, _, err := s.findingsOf(ctx, itemID)
	return fs, err
}

// findingsOf is Findings with the item they belong to.
func (s *Service) findingsOf(ctx context.Context, itemID string) ([]findings.Finding, *Item, error) {
	itemID = strings.TrimSpace(itemID)
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, nil, err
	}
	item, err := s.store.GetItem(ctx, itemID)
	if err != nil {
		return nil, nil, err
	}
	if s.findings == nil {
		return []findings.Finding{}, item, nil
	}
	fs, err := s.findings.Item(ctx, itemID)
	return fs, item, err
}

// SendBackWith names the action that can send the item's findings back to
// be fixed now — "request-changes" at sign-off, "retry" for a blocked or
// failed item whose retry starts an implementation — or "" when none can.
// Every surface that offers to send findings back asks here, so none
// offers what the action would refuse.
func (s *Service) SendBackWith(ctx context.Context, item *Item) string {
	if item == nil {
		return ""
	}
	switch item.Status {
	case StatusInReview:
		return "request-changes"
	case StatusBlocked, StatusFailed:
		if _, stage, _, _, _ := s.retryStage(ctx, item); stage == "implement" {
			return "retry"
		}
	}
	return ""
}

func findingWho(ctx context.Context, via string) findings.Who {
	actor, keyID := auth.Actor(ctx)
	return findings.Who{Actor: actor, KeyID: keyID, Via: via}
}

// checkSendBack refuses finding ids the item does not have, before the
// action that would send them back queues anything.
func (s *Service) checkSendBack(ctx context.Context, itemID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if s.findings == nil {
		return fmt.Errorf("%w: the findings record is not configured", ErrInvalidInput)
	}
	if err := s.findings.Check(ctx, itemID, ids); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidInput, strings.TrimPrefix(err.Error(), findings.ErrNotFound.Error()+": "))
	}
	return nil
}

// sentBackNotes is the findings the approver sends back, as the item's
// change requests read them: the implementer's prompt gets them from the
// record by id, and the item says in words what was asked.
func (s *Service) sentBackNotes(ctx context.Context, itemID string, ids []string) string {
	if len(ids) == 0 || s.findings == nil {
		return ""
	}
	all, err := s.findings.Item(ctx, itemID)
	if err != nil {
		return ""
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[strings.TrimSpace(id)] = true
	}
	var b strings.Builder
	for _, f := range all {
		if !want[f.ID] {
			continue
		}
		loc := ""
		if f.File != "" {
			loc = " `" + f.File + "`"
		}
		fmt.Fprintf(&b, "\n- (%s, %s)%s %s", f.Severity, f.By(), loc, f.Text)
	}
	if b.Len() == 0 {
		return ""
	}
	return "Findings to fix:" + b.String()
}

// sendBack records the findings as sent back, after the transition that
// carries them went through (the ids were checked before it). The item
// already moved and its change requests already list the findings in
// words, so a write that fails does not undo the action: it is tried once
// more and, failing again, put in the item's audit trail — the findings
// are then fixed from the change requests and not followed in the record.
func (s *Service) sendBack(ctx context.Context, item *Item, ids []string, via string) {
	if len(ids) == 0 || s.findings == nil || item == nil {
		return
	}
	sent, err := s.findings.SendBack(ctx, item.ID, ids, findingWho(ctx, via))
	if err != nil {
		sent, err = s.findings.SendBack(ctx, item.ID, ids, findingWho(ctx, via))
	}
	if err != nil {
		s.auditItem(ctx, item.ID, "inbox.findings_send_back", via, "not recorded ("+err.Error()+"): "+strings.Join(ids, ", "), "")
		return
	}
	names := make([]string, 0, len(sent))
	for _, f := range sent {
		names = append(names, f.ID)
	}
	s.auditItem(ctx, item.ID, "inbox.findings_send_back", via, fmt.Sprintf("%d sent back to be fixed: %s", len(names), strings.Join(names, ", ")), "")
}

// WaiveFinding records that the approver accepts a finding as it is: it is
// not sent to the implementer again and no review checks or blocks on it.
// Sending it back later (request changes, by id) undoes that.
func (s *Service) WaiveFinding(ctx context.Context, itemID, findingID, note, via string) (*findings.Finding, error) {
	itemID = strings.TrimSpace(itemID)
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	if _, err := s.store.GetItem(ctx, itemID); err != nil {
		return nil, err
	}
	if err := auth.RequireApprover(ctx); err != nil {
		return nil, err
	}
	if s.findings == nil {
		return nil, fmt.Errorf("%w: the findings record is not configured", ErrInvalidInput)
	}
	f, err := s.findings.Waive(ctx, itemID, findingID, note, findingWho(ctx, via))
	if err != nil {
		switch {
		case errors.Is(err, findings.ErrNotFound):
			return nil, fmt.Errorf("%w: %s", ErrNotFound, strings.TrimPrefix(err.Error(), findings.ErrNotFound.Error()+": "))
		case errors.Is(err, findings.ErrInvalidInput):
			return nil, fmt.Errorf("%w: %s", ErrInvalidInput, strings.TrimPrefix(err.Error(), findings.ErrInvalidInput.Error()+": "))
		}
		return nil, err
	}
	s.auditItem(ctx, itemID, "inbox.finding_waive", via, strings.TrimSpace(fmt.Sprintf("%s (%s, %s) %s", f.ID, f.Severity, f.By(), strings.TrimSpace(note))), "")
	return f, nil
}

// signOffFindings is the state of the item's review findings as the
// approver signs the item off, for the sign-off's audit entry: how many are
// in each state, and by id the ones that were not settled by a review —
// still to fix, claimed fixed but not confirmed, found still open — and
// the ones the approver accepted. "" for an item without findings.
func (s *Service) signOffFindings(ctx context.Context, itemID string) string {
	if s.findings == nil {
		return ""
	}
	fs, err := s.findings.Item(ctx, itemID)
	if err != nil || len(fs) == 0 {
		return ""
	}
	var parts []string
	for _, st := range []struct {
		state, word string
		ids         bool
	}{
		{findings.StateOpen, "to fix", true}, {findings.StateStillOpen, "still open", true}, {findings.StateFixed, "fixed, not confirmed", true},
		{findings.StateWaived, "waived", true}, {findings.StateAddressed, "addressed", false}, {findings.StateNoted, "not followed up", false},
	} {
		var ids []string
		for _, f := range fs {
			if f.State == st.state {
				ids = append(ids, f.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		part := fmt.Sprintf("%d %s", len(ids), st.word)
		if st.ids {
			part += " (" + strings.Join(ids, ", ") + ")"
		}
		parts = append(parts, part)
	}
	return "findings at sign-off: " + strings.Join(parts, "; ")
}

// findingCounts is the record in numbers, by state.
func findingCounts(fs []findings.Finding) map[string]int {
	out := map[string]int{}
	for _, f := range fs {
		out[f.State]++
	}
	return out
}

// ItemFindings handles GET /api/inbox/{id}/findings: the item's review
// findings, oldest first, each with its state and updates.
func (h *Handler) ItemFindings(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	fs, item, err := h.service.findingsOf(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	// send_back: the action that takes findings to send back now, for an
	// approver ("" = none does).
	sendBack := ""
	if auth.RequireApprover(r.Context()) == nil {
		sendBack = h.service.SendBackWith(r.Context(), item)
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"findings": fs, "counts": findingCounts(fs), "send_back": sendBack})
}

// WaiveFinding handles POST /api/inbox/{id}/findings/{finding}/waive: the
// approver accepts a finding as it is (approver-only).
func (h *Handler) WaiveFinding(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	// A waive without a note has no body.
	_ = rest.DecodeJSON(w, r, &req)
	f, err := h.service.WaiveFinding(r.Context(), r.PathValue("id"), r.PathValue("finding"), req.Note, viaFrom(r, ""))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, auth.ErrApproverRequired):
			rest.RespondError(w, http.StatusForbidden, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, f)
}
