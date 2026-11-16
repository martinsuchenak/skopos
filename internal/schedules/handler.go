package schedules

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// Handler serves /api/schedules.
type Handler struct {
	service *Service
	authn   *auth.Authenticator
}

func NewHandler(s *Service, authn *auth.Authenticator) *Handler {
	return &Handler{service: s, authn: authn}
}

func (h *Handler) authorized(w http.ResponseWriter, r *http.Request) bool {
	if h.authn != nil && h.authn.Authenticate(r) == nil {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func via(r *http.Request) string {
	if v := r.Header.Get("X-Skopos-Via"); v != "" {
		return v
	}
	return audit.ViaAPI
}

func respond(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, pipeline.ErrNotFound), errors.Is(err, inbox.ErrNotFound):
		rest.RespondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalidInput), errors.Is(err, pipeline.ErrInvalidInput):
		rest.RespondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		rest.RespondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrRootRequired), errors.Is(err, auth.ErrApproverRequired), errors.Is(err, auth.ErrOutOfScope):
		rest.RespondError(w, http.StatusForbidden, err.Error())
	default:
		rest.InternalError(w, err)
	}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	out, err := h.service.List(r.Context())
	if err != nil {
		respond(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	var in Input
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	sc, err := h.service.Create(r.Context(), in, via(r))
	if err != nil {
		respond(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusCreated, sc)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	sc, err := h.service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, sc)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	var in Input
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if in.OwnerKind != "" || in.OwnerID != "" {
		rest.RespondError(w, http.StatusBadRequest, "a schedule keeps its owner: create one for the other")
		return
	}
	sc, err := h.service.Update(r.Context(), r.PathValue("id"), in, via(r))
	if err != nil {
		respond(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, sc)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	if err := h.service.Delete(r.Context(), r.PathValue("id"), via(r)); err != nil {
		respond(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) Run(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	out, err := h.service.RunNow(r.Context(), r.PathValue("id"), via(r))
	if err != nil {
		respond(w, err)
		return
	}
	if out == nil {
		out = []Firing{}
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"firings": out})
}

func (h *Handler) Firings(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	out, err := h.service.Firings(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"firings": out})
}
