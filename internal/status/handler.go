package status

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/rest"
)

type Handler struct {
	service *Service
	apiKey  string
}

func NewHandler(service *Service, apiKey string) *Handler {
	return &Handler{service: service, apiKey: apiKey}
}

func (h *Handler) Report(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var input ReportInput
	if err := rest.DecodeJSON(r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	result, err := h.service.Report(r.Context(), input)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusCreated, result)
}

func (h *Handler) ListSessions(w http.ResponseWriter, r *http.Request) {
	workspaceID := r.URL.Query().Get("workspace")
	sessions, err := h.service.ListSessions(r.Context(), workspaceID)
	if err != nil {
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusOK, sessions)
}

func (h *Handler) GetSession(w http.ResponseWriter, r *http.Request) {
	session, err := h.service.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		rest.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusOK, session)
}

func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	events, err := h.service.ListEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusOK, events)
}

func (h *Handler) DeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := h.service.DeleteSession(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.apiKey == "" {
		return true
	}
	key := r.Header.Get("X-API-Key")
	if key == "" {
		key = r.URL.Query().Get("api_key")
	}
	return key == h.apiKey
}
