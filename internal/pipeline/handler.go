package pipeline

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/policy"
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
		via = audit.ViaAPI // a REST caller that names no surface
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
	case errors.Is(err, ErrPolicyRefuses):
		rest.RespondError(w, http.StatusConflict, err.Error())
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

// --- runner policy (policy.go) ---

// policyPut is the PUT body: the policy's rules, and force to save one that
// strands current settings.
type policyPut struct {
	Allow []policy.Rule `json:"allow"`
	Force bool          `json:"force,omitempty"`
	Via   string        `json:"via,omitempty"`
}

func viaOf(r *http.Request, body string) string {
	if body != "" {
		return body
	}
	if v := r.Header.Get("X-Skopos-Via"); v != "" {
		return v
	}
	return audit.ViaAPI
}

// fleetKind maps the {level} path value of /api/policy/fleet/{level}.
func fleetKind(level string) (string, bool) {
	switch level {
	case "ceiling":
		return KindFleetCeiling, true
	case "default":
		return KindFleetDefault, true
	}
	return "", false
}

// GetFleetPolicy handles GET /api/policy/fleet/{level} (ceiling | default).
func (h *Handler) GetFleetPolicy(w http.ResponseWriter, r *http.Request) {
	kind, ok := fleetKind(r.PathValue("level"))
	if !ok {
		rest.RespondError(w, http.StatusNotFound, "fleet policy level is ceiling or default")
		return
	}
	h.getPolicy(w, r, kind, "")
}

// PutFleetPolicy handles PUT /api/policy/fleet/{level}.
func (h *Handler) PutFleetPolicy(w http.ResponseWriter, r *http.Request) {
	kind, ok := fleetKind(r.PathValue("level"))
	if !ok {
		rest.RespondError(w, http.StatusNotFound, "fleet policy level is ceiling or default")
		return
	}
	h.putPolicy(w, r, kind, "")
}

// DeleteFleetPolicy handles DELETE /api/policy/fleet/{level}.
func (h *Handler) DeleteFleetPolicy(w http.ResponseWriter, r *http.Request) {
	kind, ok := fleetKind(r.PathValue("level"))
	if !ok {
		rest.RespondError(w, http.StatusNotFound, "fleet policy level is ceiling or default")
		return
	}
	h.deletePolicy(w, r, kind, "")
}

// GetWorkspacePolicy handles GET /api/workspaces/{id}/policy.
func (h *Handler) GetWorkspacePolicy(w http.ResponseWriter, r *http.Request) {
	h.getPolicy(w, r, KindWorkspace, r.PathValue("id"))
}

// PutWorkspacePolicy handles PUT /api/workspaces/{id}/policy.
func (h *Handler) PutWorkspacePolicy(w http.ResponseWriter, r *http.Request) {
	h.putPolicy(w, r, KindWorkspace, r.PathValue("id"))
}

// DeleteWorkspacePolicy handles DELETE /api/workspaces/{id}/policy.
func (h *Handler) DeleteWorkspacePolicy(w http.ResponseWriter, r *http.Request) {
	h.deletePolicy(w, r, KindWorkspace, r.PathValue("id"))
}

// GetGroupPolicy handles GET /api/groups/{id}/policy.
func (h *Handler) GetGroupPolicy(w http.ResponseWriter, r *http.Request) {
	h.getPolicy(w, r, KindGroup, r.PathValue("id"))
}

// PutGroupPolicy handles PUT /api/groups/{id}/policy.
func (h *Handler) PutGroupPolicy(w http.ResponseWriter, r *http.Request) {
	h.putPolicy(w, r, KindGroup, r.PathValue("id"))
}

// DeleteGroupPolicy handles DELETE /api/groups/{id}/policy.
func (h *Handler) DeleteGroupPolicy(w http.ResponseWriter, r *http.Request) {
	h.deletePolicy(w, r, KindGroup, r.PathValue("id"))
}

// EffectivePolicy handles GET /api/workspaces/{id}/policy/effective: every
// policy that applies to the workspace, and where each comes from.
func (h *Handler) EffectivePolicy(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	v, err := h.service.EffectivePolicyView(r.Context(), r.PathValue("id"))
	if err != nil {
		respondPolicyError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

func (h *Handler) getPolicy(w http.ResponseWriter, r *http.Request, kind, id string) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	v, err := h.service.GetPolicy(r.Context(), kind, id)
	if err != nil {
		respondPolicyError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

func (h *Handler) putPolicy(w http.ResponseWriter, r *http.Request, kind, id string) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in policyPut
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	v, err := h.service.SetPolicy(r.Context(), kind, id, policy.Policy{Allow: in.Allow}, in.Force, viaOf(r, in.Via))
	if err != nil {
		respondPolicyError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

func (h *Handler) deletePolicy(w http.ResponseWriter, r *http.Request, kind, id string) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	v, err := h.service.ClearPolicy(r.Context(), kind, id, r.URL.Query().Get("force") == "true", viaOf(r, ""))
	if err != nil {
		respondPolicyError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

func respondPolicyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrStrands):
		rest.RespondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrAdminRequired):
		rest.RespondError(w, http.StatusForbidden, err.Error())
	default:
		respondError(w, err)
	}
}
