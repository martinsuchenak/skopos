package inbox

import (
	"errors"
	"net/http"
	"strings"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// Workflow endpoints (docs/design/agent-pipeline.md §1). The human actions
// are approver-gated in the service; the handler maps auth sentinels:
// ErrApproverRequired → 403, ErrOutOfScope-shaped not-found → 404.

type actionRequest struct {
	Notes       string          `json:"notes"`
	Answer      string          `json:"answer"`
	Answers     qa.AnswersInput `json:"answers"` // approve, request-changes, retry: a list by question id, or free text
	Reason      string          `json:"reason"`
	HeadSHA     string          `json:"head_sha"`     // review gate subject on mark-done (required)
	PlanID      string          `json:"plan_id"`      // link-plan
	RevisionID  string          `json:"revision_id"`  // approve: bind to the revision Martin saw; request-changes: the plan candidate to revise
	ContentHash string          `json:"content_hash"` // approve: optional stronger binding
	BaseSHA     string          `json:"base_sha"`     // amend-plan
}

type transitionRequest struct {
	To     Status `json:"to"`
	Reason string `json:"reason"`
	// Via labels the surface for the audit log (dashboard, slack, cli, worker);
	// empty means the caller did not say.
	Via string `json:"via"`
}

// viaFrom picks the audit via label: explicit, else the X-Skopos-Via
// header (the dashboard, the CLI and the worker send theirs), else api — a
// REST caller that names no surface, such as an integration.
func viaFrom(r *http.Request, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return rest.Via(r)
}

func (h *Handler) respondTransition(w http.ResponseWriter, item *Item, err error) {
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrAlreadyConverted):
			// Already-linked items answer link-plan with a 409-class refusal,
			// not a 500 (an unmapped sentinel leaked as one, live).
			rest.RespondError(w, http.StatusConflict, err.Error())
		case errors.Is(err, auth.ErrApproverRequired):
			rest.RespondError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusForbidden, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, item)
}

func (h *Handler) Queue(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	item, err := h.service.Queue(r.Context(), r.PathValue("id"), viaFrom(r, ""))
	h.respondTransition(w, item, err)
}

// ReviewBranch handles POST /api/inbox/{id}/review: queue a review-only
// item (the agent checks and reviews its existing branch).
func (h *Handler) ReviewBranch(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	item, err := h.service.ReviewBranch(r.Context(), r.PathValue("id"), viaFrom(r, ""))
	h.respondTransition(w, item, err)
}

func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req actionRequest
	_ = rest.DecodeJSON(w, r, &req)
	// Every human approval names the revision the approver looked at (M2):
	// without it the server would bind whatever is latest at click time —
	// possibly a revision the planner wrote after the page was read. The
	// dashboard, CLI and Slack relay all send it.
	if strings.TrimSpace(req.RevisionID) == "" {
		rest.RespondError(w, http.StatusBadRequest, "revision_id is required: approve names the plan revision you reviewed (GET /api/plans/{plan_id}/revisions)")
		return
	}
	item, err := h.service.Approve(r.Context(), r.PathValue("id"), ApproveInput{
		Notes: req.Notes, Answers: req.Answers, Via: viaFrom(r, ""),
		RevisionID: req.RevisionID, ContentHash: req.ContentHash,
	})
	if errors.Is(err, ErrStaleRevision) {
		rest.RespondError(w, http.StatusConflict, err.Error())
		return
	}
	h.respondTransition(w, item, err)
}

func (h *Handler) RequestChanges(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req actionRequest
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	item, err := h.service.RequestChangesWith(r.Context(), r.PathValue("id"), ChangesInput{
		Notes: req.Notes, Answers: req.Answers, Via: viaFrom(r, ""), RevisionID: req.RevisionID,
	})
	h.respondTransition(w, item, err)
}

// SecondOpinion handles POST /api/inbox/{id}/second-opinion: queue one
// advisory review of the item's current head on a runner (approver-only).
func (h *Handler) SecondOpinion(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in SecondOpinionInput
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	in.Via = viaFrom(r, in.Via)
	req, err := h.service.RequestSecondOpinion(r.Context(), r.PathValue("id"), in)
	if err != nil {
		if errors.Is(err, ErrSecondOpinionConflict) {
			rest.RespondError(w, http.StatusConflict, err.Error())
			return
		}
		h.respondTransition(w, nil, err)
		return
	}
	rest.RespondJSON(w, http.StatusAccepted, req)
}

func (h *Handler) Retry(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req actionRequest
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	answers := req.Answers
	if a := strings.TrimSpace(req.Answer); a != "" {
		answers.Text = strings.TrimSpace(answers.Text + "\n" + a)
	}
	item, err := h.service.RetryWith(r.Context(), r.PathValue("id"), answers, viaFrom(r, ""))
	h.respondTransition(w, item, err)
}

func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req actionRequest
	_ = rest.DecodeJSON(w, r, &req)
	item, err := h.service.Reject(r.Context(), r.PathValue("id"), req.Notes, viaFrom(r, ""))
	h.respondTransition(w, item, err)
}

func (h *Handler) MarkDone(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req actionRequest
	_ = rest.DecodeJSON(w, r, &req) // empty body is fine; head_sha optional
	item, err := h.service.MarkDone(r.Context(), r.PathValue("id"), req.HeadSHA, viaFrom(r, ""))
	h.respondTransition(w, item, err)
}

// LinkPlan attaches the planner's plan to a workflow item (executor path,
// like transition: workspace scope, not approver-gated).
func (h *Handler) LinkPlan(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req actionRequest
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	item, err := h.service.LinkPlan(r.Context(), r.PathValue("id"), req.PlanID)
	h.respondTransition(w, item, err)
}

// Approvals lists the item's recorded decisions (plan and review gates).
func (h *Handler) Approvals(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	entries, err := h.service.Approvals(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, entries)
}

// Transition is the executor's system path (worker keys; scope-checked, not
// approver-gated). In 1a agent-trial calls this; in 1b the worker does.
func (h *Handler) Transition(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req transitionRequest
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !IsWorkflowStatus(req.To) {
		rest.RespondError(w, http.StatusBadRequest, "to must be a workflow status")
		return
	}
	item, err := h.service.SystemTransition(r.Context(), r.PathValue("id"), req.To, req.Reason, "", viaFrom(r, ""))
	h.respondTransition(w, item, err)
}

// AmendPlan handles POST /api/inbox/{id}/amend-plan: pause the run and
// snapshot the amended plan over the locked revision (executor path).
func (h *Handler) AmendPlan(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req actionRequest
	_ = rest.DecodeJSON(w, r, &req)
	item, err := h.service.AmendPlan(r.Context(), r.PathValue("id"), req.BaseSHA)
	h.respondTransition(w, item, err)
}

// Timeline is the item's audit-log view: every transition with actor, via,
// notes and time.
// Answer handles POST /api/inbox/{id}/answers: answers (or corrections)
// without moving the item, while it waits for the approver.
func (h *Handler) Answer(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req actionRequest
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	item, err := h.service.Answer(r.Context(), r.PathValue("id"), req.Answers, viaFrom(r, ""))
	h.respondTransition(w, item, err)
}

// Questions handles GET /api/inbox/{id}/questions: the item's questions and
// answers (internal/qa), each set in order with its questions' states.
func (h *Handler) Questions(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rec, err := h.service.Questions(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	open := 0
	for _, st := range rec.Sets {
		for _, q := range st.Questions {
			if q.State == qa.StateOpen {
				open++
			}
		}
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"sets": rec.Sets, "free": rec.Free, "open": open})
}

func (h *Handler) Timeline(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	entries, err := h.service.Timeline(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, entries)
}

type migrateRequest struct {
	WorkspaceID string        `json:"workspace_id"`
	DryRun      bool          `json:"dry_run"`
	Links       []MigrateLink `json:"links"`
}

// MigrateWorkflow handles POST /api/inbox/migrate-workflow: the one-time
// agent-trial tag→status migration (root-only).
func (h *Handler) MigrateWorkflow(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req migrateRequest
	_ = rest.DecodeJSON(w, r, &req)
	report, err := h.service.MigrateWorkflow(r.Context(), MigrateInput(req))
	if err != nil {
		if errors.Is(err, auth.ErrRootRequired) {
			rest.RespondError(w, http.StatusForbidden, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, report)
}
