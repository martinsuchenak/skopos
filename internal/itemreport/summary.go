package itemreport

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// The count of plans approved automatically: how many items had their plan
// approved by the server, how many of those then needed no input until
// sign-off, how often and why the server held a plan for the person, and how
// many items closed with nothing to do. Read from the audit trail; per
// workspace and template.

// SummaryRow is the numbers of one workspace and template ("" = items not
// started from a template).
type SummaryRow struct {
	WorkspaceID string `json:"workspace_id"`
	Template    string `json:"template"`
	// AutoApproved: items whose plan the server approved.
	AutoApproved int `json:"auto_approved"`
	// NoInputAfter: of those, signed off with no action of a person
	// between the approval and the sign-off; InputAfter: signed off after
	// one; Open: not signed off yet (or discarded).
	NoInputAfter int `json:"no_input_after"`
	InputAfter   int `json:"input_after"`
	Open         int `json:"open"`
	// Held: plans the server kept for the person, by reason (questions,
	// replan, panel, failed, other).
	Held map[string]int `json:"held"`
	// NothingToDo: items closed because the planner found nothing to do.
	NothingToDo int `json:"nothing_to_do"`
}

// Summary is the count over a period.
type Summary struct {
	Since time.Time    `json:"since"`
	Rows  []SummaryRow `json:"rows"`
	Total SummaryRow   `json:"total"`
	// Cut: there were more entries than one summary reads; the oldest of
	// the period are left out.
	Cut bool `json:"cut,omitempty"`
}

// actions after an automatic approval that count as a person's input.
var inputActions = map[string]bool{
	"inbox.request_changes": true, "inbox.retry": true, "inbox.answer": true,
	"inbox.finding_waive": true, "inbox.second_opinion": true, "inbox.check_commits": true,
}

const maxSummaryPages = 50

// heldKind sorts a hold's reason into what the count reports.
func heldKind(notes string) string {
	switch {
	case strings.HasPrefix(notes, "the planner asked"):
		return "questions"
	case strings.HasPrefix(notes, "you asked for this plan again"):
		return "replan"
	case strings.Contains(notes, "panel"), strings.Contains(notes, "several plans"):
		return "panel"
	case strings.Contains(notes, "failed"):
		return "failed"
	}
	return "other"
}

// Summary counts the items the caller may read whose plan approval, hold,
// sign-off or closing fell in the period since `since` (one workspace, or
// every one in scope for "").
func (s *Service) Summary(ctx context.Context, since time.Time, workspace string) (*Summary, error) {
	if s.Audit == nil {
		return nil, errors.New("the audit log is not wired")
	}
	if workspace != "" {
		if err := auth.RequireWorkspaceQuiet(ctx, workspace); err != nil {
			return &Summary{Since: since, Rows: []SummaryRow{}, Total: SummaryRow{Held: map[string]int{}}}, nil
		}
	}
	actions := []string{"inbox.approve", inbox.ActionAutoApproveHeld, inbox.ActionNothingToDo, "inbox.mark_done", "inbox.reject"}
	for a := range inputActions {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	out := &Summary{Since: since.UTC()}
	// A scoped key's list is merged from one page per workspace, which
	// cannot be paged as one: each workspace is read on its own.
	scopes := []string{workspace}
	if workspace == "" && auth.ScopedContext(ctx) {
		scopes = auth.PrincipalFromContext(ctx).WorkspaceList()
	}
	var entries []audit.Entry
	for _, ws := range scopes {
		before := ""
		for n := 0; ; n++ {
			if n == maxSummaryPages {
				out.Cut = true
				break
			}
			page, err := s.Audit.List(ctx, audit.ListFilter{WorkspaceID: ws, EntityType: "inbox_item", Actions: actions, Before: before, Limit: auditPage})
			if err != nil {
				return nil, err
			}
			done := len(page) < auditPage
			for _, e := range page {
				if e.CreatedAt.Before(since) {
					done = true
					break
				}
				entries = append(entries, e)
			}
			if done {
				break
			}
			before = page[len(page)-1].ID
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	type itemState struct {
		ws          string
		autoAt      string // the automatic approval's entry id ("" = none)
		input       bool
		signedOff   bool
		held        []string
		nothingToDo bool
	}
	items := map[string]*itemState{}
	var order []string
	for _, e := range entries {
		st := items[e.EntityID]
		if st == nil {
			st = &itemState{ws: e.WorkspaceID}
			items[e.EntityID] = st
			order = append(order, e.EntityID)
		}
		switch {
		case e.Action == "inbox.approve" && e.Via == audit.ViaAuto:
			st.autoAt, st.input, st.signedOff = e.ID, false, false
		case e.Action == "inbox.approve":
			// A plan approved by a person after an automatic one (an
			// amendment): input before the sign-off.
			if st.autoAt != "" && !st.signedOff {
				st.input = true
			}
		case e.Action == inbox.ActionAutoApproveHeld:
			st.held = append(st.held, heldKind(e.Notes))
		case e.Action == inbox.ActionNothingToDo:
			st.nothingToDo = true
		case e.Action == "inbox.mark_done":
			if st.autoAt != "" {
				st.signedOff = true
			}
		case inputActions[e.Action]:
			if st.autoAt != "" && !st.signedOff {
				st.input = true
			}
		}
	}

	rows := map[string]*SummaryRow{}
	row := func(ws, tmpl string) *SummaryRow {
		k := ws + "\x00" + tmpl
		if rows[k] == nil {
			rows[k] = &SummaryRow{WorkspaceID: ws, Template: tmpl, Held: map[string]int{}}
		}
		return rows[k]
	}
	out.Total.Held = map[string]int{}
	for _, id := range order {
		st := items[id]
		tmpl := ""
		if s.Items != nil {
			if it, err := s.Items.ReadItem(ctx, id); err == nil {
				tmpl = it.Template
			}
		}
		r := row(st.ws, tmpl)
		for _, add := range []*SummaryRow{r, &out.Total} {
			for _, h := range st.held {
				add.Held[h]++
			}
			if st.nothingToDo {
				add.NothingToDo++
			}
			if st.autoAt == "" {
				continue
			}
			add.AutoApproved++
			switch {
			case st.signedOff && !st.input:
				add.NoInputAfter++
			case st.signedOff:
				add.InputAfter++
			default:
				add.Open++
			}
		}
	}
	out.Rows = make([]SummaryRow, 0, len(rows))
	for _, r := range rows {
		out.Rows = append(out.Rows, *r)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].WorkspaceID != out.Rows[j].WorkspaceID {
			return out.Rows[i].WorkspaceID < out.Rows[j].WorkspaceID
		}
		return out.Rows[i].Template < out.Rows[j].Template
	})
	return out, nil
}

// Summary handles GET /api/inbox/summary[?since=30d|<RFC 3339 date>][&workspace=]:
// the count of plans approved automatically. Default: the last 30 days.
func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := parseSince(q.Get("since"), h.service.now())
	if err != nil {
		rest.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := h.service.Summary(r.Context(), since, strings.TrimSpace(q.Get("workspace")))
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, out)
}

// parseSince reads a period: "30d", "12h", or a date (2026-03-01, or RFC
// 3339); "" = the last 30 days.
func parseSince(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return now.AddDate(0, 0, -30), nil
	case strings.HasSuffix(v, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || n < 1 || n > 3660 {
			return time.Time{}, errors.New("since is a number of days (30d), hours (12h) or a date")
		}
		return now.AddDate(0, 0, -n), nil
	case strings.HasSuffix(v, "h"):
		n, err := strconv.Atoi(strings.TrimSuffix(v, "h"))
		if err != nil || n < 1 {
			return time.Time{}, errors.New("since is a number of days (30d), hours (12h) or a date")
		}
		return now.Add(-time.Duration(n) * time.Hour), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("since is a number of days (30d), hours (12h) or a date")
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
