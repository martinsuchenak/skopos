// Package itemstate answers GET /api/inbox/{id}/state: everything another
// system needs to show an item and act on it, in one read — whose turn it
// is, the plan waiting for approval, open questions, the implementation
// and its checks, the latest reviews, the run in progress, and the actions
// the caller may take now (tmp/integrations — design.md §4). It composes
// the inbox, plans, runs and questions services; it stores nothing.
package itemstate

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/itemtext"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// Items reads the item (scope-checked: a foreign item is not found).
type Items interface {
	ReadItem(ctx context.Context, id string) (*inbox.Item, error)
}

// Plans reads the plan revisions at the gate.
type Plans interface {
	LatestRevision(ctx context.Context, planID string) (*plans.Revision, error)
	OpenCandidates(ctx context.Context, planID string) ([]plans.Revision, error)
}

// Runs lists the item's runs, newest first.
type Runs interface {
	List(ctx context.Context, status runs.Status, itemID string) ([]runs.Run, error)
}

// Questions reads the item's question-and-answer record.
type Questions interface {
	Record(ctx context.Context, itemID string) (*qa.Record, error)
}

// Findings is the record of review findings (internal/findings).
type Findings interface {
	Item(ctx context.Context, itemID string) ([]findings.Finding, error)
}

// Service builds the state; Plans, Questions and Findings may be nil.
type Service struct {
	Items     Items
	Plans     Plans
	Runs      Runs
	Questions Questions
	Findings  Findings
}

// Turn and gate values.
const (
	TurnPerson = "person"
	TurnAgent  = "agent"
	TurnNone   = "none"

	GatePlanApproval = "plan_approval"
	GateSignOff      = "sign_off"
	GateBlocked      = "blocked"
	GateFailed       = "failed"
)

// State is the answer. Actions name the endpoint to POST:
// /api/inbox/{id}/<action>. A capped key sees the plan only with
// plans:read, and the reason, implementation, reviews and run only with
// runs:read — the same data as /api/plans and /api/runs.
type State struct {
	Item           ItemView        `json:"item"`
	Turn           string          `json:"turn"`
	Gate           string          `json:"gate,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Brief          string          `json:"brief"`
	Plan           *PlanView       `json:"plan,omitempty"`
	Questions      []QuestionView  `json:"questions"`
	Implementation *Implementation `json:"implementation,omitempty"`
	Reviews        []ReviewView    `json:"reviews"`
	Findings       *FindingsView   `json:"findings,omitempty"`
	Run            *RunView        `json:"run,omitempty"`
	Actions        []string        `json:"actions"`
}

type ItemView struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Title       string    `json:"title"`
	Ref         string    `json:"ref,omitempty"`
	ExternalURL string    `json:"external_url,omitempty"`
	Branch      string    `json:"branch,omitempty"`
	Flow        string    `json:"flow,omitempty"`
	Agents      string    `json:"agents,omitempty"`
	Status      string    `json:"status"`
	PlanID      string    `json:"plan_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PlanView: the revisions at the gate (the open candidates of the current
// round, else the newest revision). Approve one with its revision_id and
// content_hash.
type PlanView struct {
	PlanID    string         `json:"plan_id"`
	Revisions []RevisionView `json:"revisions"`
}

type RevisionView struct {
	RevisionID  string               `json:"revision_id"`
	RevisionNo  int                  `json:"revision_no"`
	Label       string               `json:"label,omitempty"`
	ContentHash string               `json:"content_hash"`
	BaseSHA     string               `json:"base_sha,omitempty"`
	Locked      bool                 `json:"locked"`
	Skills      []string             `json:"skills,omitempty"`
	Steps       []plans.RevisionStep `json:"steps"`
}

// QuestionView is one open question; answer it through /answers (or with
// approve, request-changes, retry) by id.
type QuestionView struct {
	ID       string   `json:"id"`
	SetID    string   `json:"set_id"`
	Asker    string   `json:"asker"`
	Label    string   `json:"label,omitempty"`
	Text     string   `json:"text"`
	Options  []string `json:"options"`
	Blocking bool     `json:"blocking"`
}

// Implementation is the newest implement run's outcome.
type Implementation struct {
	RunID      string       `json:"run_id"`
	Status     string       `json:"status"`
	HeadSHA    string       `json:"head_sha,omitempty"`
	Pushed     *bool        `json:"pushed,omitempty"`
	PRURL      string       `json:"pr_url,omitempty"`
	Diffstat   string       `json:"diffstat,omitempty"`
	Summary    string       `json:"summary,omitempty"`
	FailReason string       `json:"fail_reason,omitempty"`
	Setup      []runs.Check `json:"setup,omitempty"`
	Checks     []runs.Check `json:"checks,omitempty"`
	FixRounds  int          `json:"fix_rounds,omitempty"`
}

// ReviewView is one finished review of the newest reviewed head (a panel
// member, a single reviewer, a review-only run or a second opinion).
// Passed: it succeeded with no blocking finding.
type ReviewView struct {
	RunID     string         `json:"run_id"`
	Stage     string         `json:"stage"`
	Runner    string         `json:"runner,omitempty"`
	Focus     string         `json:"focus,omitempty"`
	HeadSHA   string         `json:"head_sha,omitempty"`
	Status    string         `json:"status"`
	Passed    bool           `json:"passed"`
	Blocking  int            `json:"blocking"`
	StillOpen int            `json:"still_open,omitempty"`
	Nits      int            `json:"nits"`
	Summary   string         `json:"summary,omitempty"`
	Findings  []runs.Finding `json:"findings"`
}

// FindingsView is the item's review findings in short: how many are in each
// state, and the ones something is still owed on (to fix, claimed fixed,
// found still open). GET /api/inbox/{id}/findings is the full record.
type FindingsView struct {
	Counts map[string]int `json:"counts"`
	Open   []FindingView  `json:"open"`
}

// FindingView is one finding something is still owed on.
type FindingView struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Severity string `json:"severity"`
	Text     string `json:"text"`
	File     string `json:"file,omitempty"`
	By       string `json:"by"`
	HeadSHA  string `json:"head_sha,omitempty"`
}

// findingsView is nil for an item without findings.
func findingsView(all []findings.Finding) *FindingsView {
	if len(all) == 0 {
		return nil
	}
	v := &FindingsView{Counts: map[string]int{}, Open: []FindingView{}}
	for _, f := range all {
		v.Counts[f.State]++
		switch f.State {
		case findings.StateOpen, findings.StateFixed, findings.StateStillOpen:
			v.Open = append(v.Open, FindingView{ID: f.ID, State: f.State, Severity: f.Severity, Text: f.Text, File: f.File, By: f.By(), HeadSHA: f.HeadSHA})
		}
	}
	return v
}

// RunView is the run in progress (queued, claimed, running, paused or
// interrupted).
type RunView struct {
	ID        string    `json:"id"`
	Stage     string    `json:"stage"`
	Status    string    `json:"status"`
	Runner    string    `json:"runner,omitempty"`
	Phase     string    `json:"phase,omitempty"`
	WorkerID  string    `json:"worker_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Get builds the item's state for the caller.
func (s *Service) Get(ctx context.Context, itemID string) (*State, error) {
	item, err := s.Items.ReadItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	brief, _ := itemtext.Split(item.Content)
	st := &State{
		Item: ItemView{
			ID: item.ID, WorkspaceID: item.WorkspaceID, Title: item.Title, Ref: item.Ref, ExternalURL: item.ExternalURL,
			Branch: item.Branch, Flow: item.Flow, Agents: item.Agents, Status: string(item.Status), PlanID: item.PlanID,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		},
		Brief:     strings.TrimSpace(brief),
		Questions: []QuestionView{},
		Reviews:   []ReviewView{},
	}
	st.Turn, st.Gate = turnAndGate(item.Status)

	var all []runs.Run
	if s.Runs != nil {
		if all, err = s.Runs.List(ctx, "", item.ID); err != nil {
			return nil, err
		}
	}
	p := auth.PrincipalFromContext(ctx)
	if p.Allows(auth.AreaRuns, false) {
		st.Run = current(all)
		st.Implementation = implementation(all)
		st.Reviews = reviews(all)
		if st.Gate == GateBlocked || st.Gate == GateFailed {
			st.Reason = failReason(all)
		}
		// Findings come from runs: the same capability as the reviews.
		if s.Findings != nil {
			fs, err := s.Findings.Item(ctx, item.ID)
			if err != nil {
				return nil, err
			}
			st.Findings = findingsView(fs)
		}
	}
	if item.PlanID != "" && s.Plans != nil && p.Allows(auth.AreaPlans, false) {
		if st.Plan, err = s.plan(ctx, item.PlanID); err != nil {
			return nil, err
		}
	}
	if s.Questions != nil {
		rec, err := s.Questions.Record(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		st.Questions = openQuestions(rec)
	}
	st.Actions = Actions(ctx, item, all)
	return st, nil
}

// turnAndGate: whose move it is, and which gate the item waits at.
func turnAndGate(status inbox.Status) (string, string) {
	switch status {
	case inbox.StatusAwaitingApproval:
		return TurnPerson, GatePlanApproval
	case inbox.StatusInReview:
		return TurnPerson, GateSignOff
	case inbox.StatusBlocked:
		return TurnPerson, GateBlocked
	case inbox.StatusFailed:
		return TurnPerson, GateFailed
	case inbox.StatusQueued, inbox.StatusPlanning, inbox.StatusApproved, inbox.StatusImplementing:
		return TurnAgent, ""
	case inbox.StatusOpen, inbox.StatusInProgress:
		return TurnPerson, "" // not started: someone queues it
	}
	return TurnNone, ""
}

// Actions are the workflow actions the caller may take on the item now,
// named as their endpoints. They need the approver permission, so a key
// without it gets none. itemRuns (newest first) decide second-opinion, by
// the rule the request itself checks (inbox.SecondOpinionHead).
func Actions(ctx context.Context, item *inbox.Item, itemRuns []runs.Run) []string {
	if auth.RequireApprover(ctx) != nil {
		return []string{}
	}
	switch item.Status {
	case inbox.StatusOpen, inbox.StatusInProgress:
		if item.WorkspaceID == "" {
			return []string{}
		}
		if item.Branch != "" || item.Ref != "" {
			return []string{"queue", "review"}
		}
		return []string{"queue"}
	case inbox.StatusAwaitingApproval:
		return []string{"approve", "request-changes", "answers", "reject"}
	case inbox.StatusInReview:
		return withSecondOpinion(item, itemRuns, []string{"mark-done", "request-changes"}, "reject")
	case inbox.StatusBlocked:
		return withSecondOpinion(item, itemRuns, []string{"retry", "answers"}, "reject")
	case inbox.StatusFailed:
		return []string{"retry", "answers", "reject"}
	case inbox.StatusQueued, inbox.StatusPlanning, inbox.StatusApproved, inbox.StatusImplementing:
		return []string{"reject"}
	}
	return []string{}
}

// withSecondOpinion lists before, second-opinion when one can be
// requested now, then after.
func withSecondOpinion(item *inbox.Item, itemRuns []runs.Run, before []string, after ...string) []string {
	summaries := make([]inbox.RunSummary, 0, len(itemRuns))
	for _, r := range itemRuns {
		summaries = append(summaries, inbox.RunSummary{ID: r.ID, Stage: r.Stage, Status: string(r.Status),
			Runner: r.Runner, Branch: r.Branch, HeadSHA: r.HeadSHA})
	}
	if _, _, err := inbox.SecondOpinionHead(item, summaries); err == nil {
		before = append(before, "second-opinion")
	}
	return append(before, after...)
}

func (s *Service) plan(ctx context.Context, planID string) (*PlanView, error) {
	revs, err := s.Plans.OpenCandidates(ctx, planID)
	if err != nil && !errors.Is(err, plans.ErrNotFound) {
		return nil, err
	}
	if len(revs) == 0 {
		latest, err := s.Plans.LatestRevision(ctx, planID)
		if errors.Is(err, plans.ErrNotFound) {
			return &PlanView{PlanID: planID, Revisions: []RevisionView{}}, nil
		}
		if err != nil {
			return nil, err
		}
		revs = []plans.Revision{*latest}
	}
	pv := &PlanView{PlanID: planID, Revisions: make([]RevisionView, 0, len(revs))}
	for _, r := range revs {
		steps := r.Steps
		if steps == nil {
			steps = []plans.RevisionStep{}
		}
		pv.Revisions = append(pv.Revisions, RevisionView{
			RevisionID: r.ID, RevisionNo: r.RevisionNo, Label: r.Label, ContentHash: r.ContentHash,
			BaseSHA: r.BaseSHA, Locked: r.LockedAt != nil, Skills: r.Skills, Steps: steps,
		})
	}
	return pv, nil
}

func openQuestions(rec *qa.Record) []QuestionView {
	out := []QuestionView{}
	if rec == nil {
		return out
	}
	for _, set := range rec.Sets {
		if !set.Open {
			continue
		}
		for _, q := range set.Questions {
			if q.State != qa.StateOpen {
				continue
			}
			opts := q.Options
			if opts == nil {
				opts = []string{}
			}
			out = append(out, QuestionView{ID: q.ID, SetID: set.ID, Asker: set.Asker, Label: set.Label,
				Text: q.Text, Options: opts, Blocking: set.Blocking})
		}
	}
	return out
}

// active run statuses: work not finished yet (an interrupted run is
// re-claimed by its owner).
func active(s runs.Status) bool {
	switch s {
	case runs.StatusQueued, runs.StatusClaimed, runs.StatusRunning, runs.StatusPaused, runs.StatusInterrupted:
		return true
	}
	return false
}

// current is the newest unfinished run (runs are newest first).
func current(all []runs.Run) *RunView {
	for _, r := range all {
		if active(r.Status) {
			return &RunView{ID: r.ID, Stage: r.Stage, Status: string(r.Status), Runner: r.Runner,
				Phase: r.Phase, WorkerID: r.WorkerID, CreatedAt: r.CreatedAt}
		}
	}
	return nil
}

// implementation is the newest finished implement run.
func implementation(all []runs.Run) *Implementation {
	for _, r := range all {
		if r.Stage != runs.StageImplement || active(r.Status) || r.Status == runs.StatusCancelled {
			continue
		}
		return &Implementation{RunID: r.ID, Status: string(r.Status), HeadSHA: r.HeadSHA, Pushed: r.Pushed,
			PRURL: r.PRURL, Diffstat: r.Diffstat, Summary: r.ResultSummary, FailReason: r.FailReason,
			Setup: r.Setup, Checks: r.Checks, FixRounds: r.FixRounds}
	}
	return nil
}

func reviewStage(stage string) bool {
	switch stage {
	case runs.StageReview, runs.StageBranchReview, runs.StageSecondOpinion:
		return true
	}
	return false
}

// reviews are the finished reviews of the newest reviewed head (only the
// newest review when it names no head).
func reviews(all []runs.Run) []ReviewView {
	out := []ReviewView{}
	head := ""
	for _, r := range all {
		if !reviewStage(r.Stage) || (r.Status != runs.StatusSucceeded && r.Status != runs.StatusFailed) {
			continue
		}
		if len(out) == 0 {
			head = r.HeadSHA
		} else if head == "" || r.HeadSHA != head {
			continue
		}
		v := ReviewView{RunID: r.ID, Stage: r.Stage, Runner: r.Runner, Focus: r.Focus, HeadSHA: r.HeadSHA,
			Status: string(r.Status), Summary: r.ResultSummary, Findings: r.Findings}
		if v.Findings == nil {
			v.Findings = []runs.Finding{}
		}
		// Blocking counts the earlier findings the review found still open
		// too (StillOpen); nits are only among its own.
		v.Blocking = runs.BlockingFindings(r)
		v.StillOpen = r.StillOpen
		v.Nits = len(r.Findings) - (v.Blocking - r.StillOpen)
		v.Passed = runs.ReviewPassed(r)
		out = append(out, v)
	}
	return out
}

// failReason is why the current round stopped: the newest failed run's
// reason, looking no further back than the round's start (a succeeded
// plan or implement run) — an older round's failure was retried already.
func failReason(all []runs.Run) string {
	for _, r := range all {
		if r.Status == runs.StatusFailed && r.FailReason != "" {
			return r.FailReason
		}
		if r.Status == runs.StatusSucceeded && (r.Stage == runs.StagePlan || r.Stage == runs.StageImplement) {
			return ""
		}
	}
	return ""
}

// Handler serves GET /api/inbox/{id}/state.
type Handler struct{ service *Service }

func NewHandler(s *Service) *Handler { return &Handler{service: s} }

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	st, err := h.service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, inbox.ErrNotFound), errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, "item not found")
		case errors.Is(err, inbox.ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, st)
}
