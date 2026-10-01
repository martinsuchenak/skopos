package inbox

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// Workflow endpoints (docs/design/agent-pipeline.md §1). The human actions
// are approver-gated in the service; the handler maps auth sentinels:
// ErrApproverRequired → 403, ErrOutOfScope-shaped not-found → 404.

type actionRequest struct {
	Notes   string `json:"notes"`
	Answer  string `json:"answer"`
	Reason  string `json:"reason"`
	HeadSHA string `json:"head_sha"` // review gate subject on mark-done
	PlanID  string `json:"plan_id"`  // link-plan
}

type transitionRequest struct {
	To     Status `json:"to"`
	Reason string `json:"reason"`
	// Via labels the surface for the audit log (dashboard, slack, cli, worker);
	// empty means the caller did not say.
	Via string `json:"via"`
}

// viaFrom picks the audit via label: explicit, else the default for REST.
func viaFrom(r *http.Request, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if r.Header.Get("X-Skopos-Via") != "" {
		return r.Header.Get("X-Skopos-Via")
	}
	return audit.ViaCLI
}

func (h *Handler) respondTransition(w http.ResponseWriter, item *Item, err error) {
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
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

func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req actionRequest
	_ = rest.DecodeJSON(w, r, &req) // empty body is fine
	item, err := h.service.Approve(r.Context(), r.PathValue("id"), req.Notes, viaFrom(r, ""))
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
	item, err := h.service.RequestChanges(r.Context(), r.PathValue("id"), req.Notes, viaFrom(r, ""))
	h.respondTransition(w, item, err)
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
	item, err := h.service.Retry(r.Context(), r.PathValue("id"), req.Answer, viaFrom(r, ""))
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

// Timeline is the item's audit-log view: every transition with actor, via,
// notes and time.
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
	DryRun bool `json:"dry_run"`
}

// MigrateWorkflow handles POST /api/inbox/migrate-workflow: the one-time
// agent-trial tag→status migration (root-only).
func (h *Handler) MigrateWorkflow(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req migrateRequest
	_ = rest.DecodeJSON(w, r, &req) // empty body = apply
	report, err := h.service.MigrateWorkflow(r.Context(), req.DryRun)
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
