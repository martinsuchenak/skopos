package blackboard

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

type Handler struct {
	service *Service
	apiKey  string
}

func NewHandler(service *Service, apiKey string) *Handler {
	return &Handler{service: service, apiKey: apiKey}
}

func (h *Handler) authorized(r *http.Request) bool {
	return auth.Authorize(r, h.apiKey)
}

// WriteEntry handles POST /api/blackboard/entries.
func (h *Handler) WriteEntry(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var input WriteInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	result, err := h.service.Write(r.Context(), input)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusCreated, result)
}

// ReadBundle handles GET /api/blackboard/entries.
func (h *Handler) ReadBundle(w http.ResponseWriter, r *http.Request) {
	workspaceID := r.URL.Query().Get("workspace")
	branchName := r.URL.Query().Get("branch")
	sessionID := r.URL.Query().Get("session_id")

	bundle, err := h.service.Bundle(r.Context(), workspaceID, branchName, sessionID)
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, bundle)
}

// Promote handles PATCH /api/blackboard/entries/{id}/promote.
func (h *Handler) Promote(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	id := r.PathValue("id")
	if err := h.service.Promote(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrAlreadyAtTopScope):
			rest.RespondError(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Delete handles DELETE /api/blackboard/entries/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	id := r.PathValue("id")
	if err := h.service.Delete(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
