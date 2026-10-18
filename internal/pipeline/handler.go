package pipeline

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/audit"
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

func (h *Handler) authorized(r *http.Request) bool { return h.authn.Authenticate(r) != nil }

// putInput is the PUT body: the owner's whole settings document (stages it
// omits are cleared at this level) plus the surface label for the audit log.
type putInput struct {
	Settings
	Via string `json:"via,omitempty"`
}

// GetWorkspace handles GET /api/workspaces/{id}/pipeline.
func (h *Handler) GetWorkspace(w http.ResponseWriter, r *http.Request) { h.get(w, r, KindWorkspace) }

// PutWorkspace handles PUT /api/workspaces/{id}/pipeline.
func (h *Handler) PutWorkspace(w http.ResponseWriter, r *http.Request) { h.put(w, r, KindWorkspace) }

// GetGroup handles GET /api/groups/{id}/pipeline.
func (h *Handler) GetGroup(w http.ResponseWriter, r *http.Request) { h.get(w, r, KindGroup) }

// PutGroup handles PUT /api/groups/{id}/pipeline.
func (h *Handler) PutGroup(w http.ResponseWriter, r *http.Request) { h.put(w, r, KindGroup) }

func (h *Handler) get(w http.ResponseWriter, r *http.Request, kind string) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	v, err := h.service.Get(r.Context(), kind, r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request, kind string) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in putInput
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	via := in.Via
	if via == "" {
		via = r.Header.Get("X-Skopos-Via")
	}
	if via == "" {
		via = audit.ViaCLI
	}
	v, err := h.service.Put(r.Context(), kind, r.PathValue("id"), in.Settings, via)
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

func respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		rest.RespondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalidInput):
		rest.RespondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		rest.RespondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrRootRequired), errors.Is(err, auth.ErrApproverRequired), errors.Is(err, auth.ErrOutOfScope):
		rest.RespondError(w, http.StatusForbidden, err.Error())
	default:
		rest.InternalError(w, err)
	}
}
