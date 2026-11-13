package wsconfig

import (
	"encoding/json"
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

func (h *Handler) authorized(w http.ResponseWriter, r *http.Request) bool {
	if h.authn.Authenticate(r) == nil {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

// putInput is the PUT body: the owner's whole config (a field left out is
// not set at this level), and an optional note kept with the revision.
type putInput struct {
	// Config is required: a body without it is a mistake, not an empty
	// config (which would manage a workspace with every default).
	Config *Config `json:"config"`
	Note   string  `json:"note,omitempty"`
	// IfRev makes the save conditional on the stored revision still being
	// this one ("" = none stored): a read-modify-write that lost a race is
	// refused with 409.
	IfRev *string `json:"if_rev,omitempty"`
}

func respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		rest.RespondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotFound):
		rest.RespondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict):
		rest.RespondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrAdminRequired):
		rest.RespondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrNoSecretKey):
		rest.RespondError(w, http.StatusConflict, err.Error())
	default:
		rest.InternalError(w, err)
	}
}

// GetWorkspace handles GET /api/workspaces/{id}/config.
func (h *Handler) GetWorkspace(w http.ResponseWriter, r *http.Request) { h.get(w, r, KindWorkspace) }

// PutWorkspace handles PUT /api/workspaces/{id}/config.
func (h *Handler) PutWorkspace(w http.ResponseWriter, r *http.Request) { h.put(w, r, KindWorkspace) }

// DeleteWorkspace handles DELETE /api/workspaces/{id}/config.
func (h *Handler) DeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	h.del(w, r, KindWorkspace)
}

// GetGroup handles GET /api/groups/{id}/config.
func (h *Handler) GetGroup(w http.ResponseWriter, r *http.Request) { h.get(w, r, KindGroup) }

// PutGroup handles PUT /api/groups/{id}/config.
func (h *Handler) PutGroup(w http.ResponseWriter, r *http.Request) { h.put(w, r, KindGroup) }

// DeleteGroup handles DELETE /api/groups/{id}/config.
func (h *Handler) DeleteGroup(w http.ResponseWriter, r *http.Request) { h.del(w, r, KindGroup) }

func (h *Handler) get(w http.ResponseWriter, r *http.Request, kind string) {
	if !h.authorized(w, r) {
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
	if !h.authorized(w, r) {
		return
	}
	var in putInput
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if in.Config == nil {
		rest.RespondError(w, http.StatusBadRequest, `the body is {"config": {…}}: the owner's whole config`)
		return
	}
	var v *View
	var err error
	if in.IfRev != nil {
		v, err = h.service.SetIf(r.Context(), kind, r.PathValue("id"), *in.Config, rest.Via(r), in.Note, *in.IfRev)
	} else {
		v, err = h.service.Set(r.Context(), kind, r.PathValue("id"), *in.Config, rest.Via(r), in.Note)
	}
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

// checkInput is the body of a check: the config as a save would send it,
// read field by field (so one bad value does not hide the others).
type checkInput struct {
	Config json.RawMessage `json:"config"`
}

// CheckWorkspace handles POST /api/workspaces/{id}/config/check.
func (h *Handler) CheckWorkspace(w http.ResponseWriter, r *http.Request) {
	h.check(w, r, KindWorkspace)
}

// CheckGroup handles POST /api/groups/{id}/config/check.
func (h *Handler) CheckGroup(w http.ResponseWriter, r *http.Request) { h.check(w, r, KindGroup) }

func (h *Handler) check(w http.ResponseWriter, r *http.Request, kind string) {
	if !h.authorized(w, r) {
		return
	}
	var in checkInput
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil || len(in.Config) == 0 {
		rest.RespondError(w, http.StatusBadRequest, `the body is {"config": {…}}: the config a save would send`)
		return
	}
	res, err := h.service.Check(r.Context(), kind, r.PathValue("id"), in.Config)
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

func (h *Handler) del(w http.ResponseWriter, r *http.Request, kind string) {
	if !h.authorized(w, r) {
		return
	}
	v, err := h.service.Clear(r.Context(), kind, r.PathValue("id"), rest.Via(r))
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

// Effective handles GET /api/workspaces/{id}/config/effective: what applies
// to the workspace now — nothing when it is not managed.
func (h *Handler) Effective(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	eff, err := h.service.EffectiveFor(r.Context(), r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, eff)
}

// At handles GET /api/workspaces/{id}/config/{rev}: the effective config a
// revision id names — what a run claimed with it executes under.
func (h *Handler) At(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	eff, err := h.service.At(r.Context(), r.PathValue("id"), r.PathValue("rev"))
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, eff)
}

// FleetConfig handles GET /api/fleet/config: the runner and wrapper
// definitions and the revision that names them.
func (h *Handler) FleetConfig(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	f, err := h.service.Fleet(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, f)
}

// ListRunners handles GET /api/fleet/runners.
func (h *Handler) ListRunners(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	out, err := h.service.Runners(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"runners": out})
}

// PutRunner handles PUT /api/fleet/runners/{name}.
func (h *Handler) PutRunner(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	var in RunnerDef
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	in.Name = r.PathValue("name")
	v, _, err := h.service.SetRunner(r.Context(), in, rest.Via(r))
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

// DeleteRunner handles DELETE /api/fleet/runners/{name}.
func (h *Handler) DeleteRunner(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	if _, err := h.service.DeleteRunner(r.Context(), r.PathValue("name"), rest.Via(r)); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListWrappers handles GET /api/fleet/wrappers.
func (h *Handler) ListWrappers(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	out, err := h.service.Wrappers(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}
	// Who names each wrapper is shown to callers that could change it.
	if auth.RequireFleetAdmin(r.Context()) == nil {
		if users, err := h.service.WrapperUsers(r.Context()); err == nil {
			for i := range out {
				out[i].UsedBy = users[out[i].Name]
			}
		}
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"wrappers": out})
}

// PutWrapper handles PUT /api/fleet/wrappers/{name}.
func (h *Handler) PutWrapper(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	var in WrapperDef
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	in.Name = r.PathValue("name")
	v, _, err := h.service.SetWrapper(r.Context(), in, rest.Via(r))
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

// DeleteWrapper handles DELETE /api/fleet/wrappers/{name} (?force=1 while
// stage blocks still name it).
func (h *Handler) DeleteWrapper(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if _, err := h.service.DeleteWrapper(r.Context(), r.PathValue("name"), rest.Via(r), force); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListMCPServers handles GET /api/fleet/mcp-servers: the definitions and
// which of their secrets are set. Never a value.
func (h *Handler) ListMCPServers(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	out, err := h.service.MCPServers(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"mcp_servers": out})
}

// PutMCPServer handles PUT /api/fleet/mcp-servers/{name}.
func (h *Handler) PutMCPServer(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	var in MCPServerDef
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	in.Name = r.PathValue("name")
	v, _, err := h.service.SetMCPServer(r.Context(), in, rest.Via(r))
	if err != nil {
		respondError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, v)
}

// DeleteMCPServer handles DELETE /api/fleet/mcp-servers/{name} (?force=1
// while a config still gives its runs the server).
func (h *Handler) DeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if _, err := h.service.DeleteMCPServer(r.Context(), r.PathValue("name"), rest.Via(r), force); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PutMCPSecret handles PUT /api/fleet/mcp-servers/{name}/secrets/{secret}:
// the body carries the value, the answer never does.
func (h *Handler) PutMCPSecret(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	var in struct {
		Value string `json:"value"`
	}
	if err := rest.DecodeJSONStrict(w, r, &in); err != nil {
		// Not the decoder's message: it can quote the body.
		rest.RespondError(w, http.StatusBadRequest, `invalid request body: send {"value": "…"}`)
		return
	}
	if err := h.service.SetMCPSecret(r.Context(), r.PathValue("name"), r.PathValue("secret"), []byte(in.Value), rest.Via(r)); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteMCPSecret handles DELETE /api/fleet/mcp-servers/{name}/secrets/{secret}.
func (h *Handler) DeleteMCPSecret(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	if err := h.service.UnsetMCPSecret(r.Context(), r.PathValue("name"), r.PathValue("secret"), rest.Via(r)); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
