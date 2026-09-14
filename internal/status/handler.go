package status

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

type Handler struct {
	service *Service
	authn   *auth.Authenticator
}

func NewHandler(service *Service, authn *auth.Authenticator) *Handler {
	return &Handler{service: service, authn: authn}
}

func (h *Handler) authorized(r *http.Request) bool {
	return h.authn.Authenticate(r) != nil
}

func (h *Handler) Report(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var input ReportInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	result, err := h.service.Report(r.Context(), input)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, ErrNotFound):
			// Attaching to a foreign session is indistinguishable from an
			// unknown one (uniform 404, no existence oracle).
			rest.RespondError(w, http.StatusNotFound, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusCreated, result)
}

func (h *Handler) ListSessions(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	workspaceID := rest.QueryAlias(r, "workspace_id", "workspace")
	sessions, err := h.service.ListSessions(r.Context(), workspaceID)
	if err != nil {
		if errors.Is(err, auth.ErrOutOfScope) {
			rest.RespondError(w, http.StatusForbidden, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	if sessions == nil {
		sessions = []SessionSummary{}
	}
	rest.RespondJSON(w, http.StatusOK, sessions)
}

func (h *Handler) GetSession(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	session, err := h.service.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, session)
}

func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	events, err := h.service.ListEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNotFound), errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	if events == nil {
		events = []Event{}
	}
	rest.RespondJSON(w, http.StatusOK, events)
}

func (h *Handler) DeleteSession(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.service.DeleteSession(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
