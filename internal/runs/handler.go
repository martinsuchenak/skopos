package runs

import (
	"errors"
	"net/http"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// Handler serves the worker-facing queue API. Auth is the standard bearer
// middleware; scope enforcement lives in the service (a worker key sees
// exactly its workspaces' runs).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) respondRunError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		rest.RespondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
		rest.RespondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrLeaseHeld):
		rest.RespondError(w, http.StatusConflict, err.Error())
	default:
		rest.InternalError(w, err)
	}
}

// Claim handles POST /api/runs/claim: the worker's poll. 204 when nothing
// matches — the honest answer, so an idle worker distinguishes "empty queue"
// from "gone away".
func (h *Handler) Claim(w http.ResponseWriter, r *http.Request) {
	var in ClaimInput
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, err := h.service.ClaimNext(r.Context(), in)
	if err != nil {
		h.respondRunError(w, err, "claim")
		return
	}
	if run == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rest.RespondJSON(w, http.StatusOK, run)
}

// Heartbeat handles POST /api/runs/{id}/heartbeat.
func (h *Handler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkerID string `json:"worker_id"`
	}
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.Heartbeat(r.Context(), r.PathValue("id"), in.WorkerID); err != nil {
		h.respondRunError(w, err, "heartbeat")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Report handles POST /api/runs/{id}/report.
func (h *Handler) Report(w http.ResponseWriter, r *http.Request) {
	var in ReportInput
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, err := h.service.Report(r.Context(), r.PathValue("id"), in)
	if err != nil {
		h.respondRunError(w, err, "report")
		return
	}
	rest.RespondJSON(w, http.StatusOK, run)
}

// Pause handles POST /api/runs/{id}/pause. A resume_after (quota resets)
// makes the pause restart-safe: the owner reclaims it via any claim once
// the time passes.
func (h *Handler) Pause(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkerID    string `json:"worker_id"`
		Reason      string `json:"reason"`
		ResumeAfter string `json:"resume_after"`
	}
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var resume time.Time
	if in.ResumeAfter != "" {
		if t, err := time.Parse(time.RFC3339Nano, in.ResumeAfter); err == nil {
			resume = t
		}
	}
	if err := h.service.Pause(r.Context(), r.PathValue("id"), in.WorkerID, in.Reason, resume); err != nil {
		h.respondRunError(w, err, "pause")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Cancel handles POST /api/runs/{id}/cancel: the owning worker retires its
// own run (an abandoned stage — the item left its phase — is a
// cancellation, not a failure; A4).
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkerID string `json:"worker_id"`
		Reason   string `json:"reason"`
	}
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.CancelOwn(r.Context(), r.PathValue("id"), in.WorkerID, in.Reason); err != nil {
		h.respondRunError(w, err, "cancel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Resume handles POST /api/runs/{id}/resume.
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkerID string `json:"worker_id"`
	}
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.Resume(r.Context(), r.PathValue("id"), in.WorkerID); err != nil {
		h.respondRunError(w, err, "resume")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Get handles GET /api/runs/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	run, err := h.service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.respondRunError(w, err, "get")
		return
	}
	rest.RespondJSON(w, http.StatusOK, run)
}

// List handles GET /api/runs?status=&item_id=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	runs, err := h.service.List(r.Context(), Status(r.URL.Query().Get("status")), r.URL.Query().Get("item_id"))
	if err != nil {
		h.respondRunError(w, err, "list")
		return
	}
	if runs == nil {
		runs = []Run{}
	}
	rest.RespondJSON(w, http.StatusOK, runs)
}
