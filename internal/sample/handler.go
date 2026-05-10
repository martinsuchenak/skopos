package sample

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/rest"
)

type Handler struct {
	service *Service
}

func NewHandler(s *Service) *Handler {
	return &Handler{service: s}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	items, err := h.service.List(r.Context())
	if err != nil {
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusOK, items)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	item, err := h.service.Get(r.Context(), id)
	if err != nil {
		rest.RespondError(w, http.StatusNotFound, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusOK, item)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var input CreateSampleInput
	if err := rest.DecodeJSON(r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	item, err := h.service.Create(r.Context(), input)
	if err != nil {
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusCreated, item)
}
