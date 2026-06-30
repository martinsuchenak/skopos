package workspaces

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

func (h *Handler) authorized(r *http.Request) bool { return auth.Authorize(r, h.apiKey) }

// Create handles POST /api/workspaces.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var input CreateInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ws, err := h.service.Create(r.Context(), input)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusCreated, ws)
}

// List handles GET /api/workspaces.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.service.List(r.Context())
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	if list == nil {
		list = []Workspace{}
	}
	rest.RespondJSON(w, http.StatusOK, list)
}

// Delete handles DELETE /api/workspaces/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.service.Delete(r.Context(), r.PathValue("id")); err != nil {
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
