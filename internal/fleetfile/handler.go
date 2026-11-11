package fleetfile

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

type Handler struct {
	service *Service
	authn   *auth.Authenticator
}

func NewHandler(service *Service, authn *auth.Authenticator) *Handler {
	return &Handler{service: service, authn: authn}
}

func respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrAdminRequired), errors.Is(err, auth.ErrRootRequired), errors.Is(err, auth.ErrOutOfScope), errors.Is(err, auth.ErrApproverRequired):
		rest.RespondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, pipeline.ErrStrands), errors.Is(err, pipeline.ErrConflict), errors.Is(err, pipeline.ErrPolicyRefuses), errors.Is(err, wsconfig.ErrConflict):
		rest.RespondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, pipeline.ErrNotFound), errors.Is(err, wsconfig.ErrNotFound), errors.Is(err, apikeys.ErrNotFound):
		rest.RespondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalid), errors.Is(err, pipeline.ErrInvalidInput), errors.Is(err, wsconfig.ErrInvalid),
		errors.Is(err, apikeys.ErrInvalidInput), errors.Is(err, workspaces.ErrInvalidInput):
		rest.RespondError(w, http.StatusBadRequest, err.Error())
	default:
		rest.InternalError(w, err)
	}
}

// Export handles GET /api/config/export: everything as one document.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	if h.authn.Authenticate(r) == nil {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	doc, err := h.service.Export(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, doc)
}

// Apply handles POST /api/config/apply: make the server match a document,
// or with "plan": true list what that would change.
func (h *Handler) Apply(w http.ResponseWriter, r *http.Request) {
	if h.authn.Authenticate(r) == nil {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in Input
	// Strict: in a document that is authoritative for what it lists, a
	// misspelt section would read as one left out, which clears it.
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	res, err := h.service.Apply(r.Context(), in)
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}
