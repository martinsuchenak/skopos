package apikeys

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/workspaces"
)

type Handler struct {
	service    *Service
	registry   *workspaces.Service
}

func NewHandler(service *Service, registry *workspaces.Service) *Handler {
	return &Handler{service: service, registry: registry}
}

// requireRoot gates key management: only the root key may mint or revoke
// keys. The router middleware has already authenticated the request; the
// nil check keeps the handler safe for direct (test) invocation.
func (h *Handler) requireRoot(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.PrincipalFromContext(r.Context())
	if p == nil {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return nil
	}
	if !p.IsRoot() {
		rest.RespondError(w, http.StatusForbidden, "key management requires the root key")
		return nil
	}
	return p
}

type createRequest struct {
	Name       string   `json:"name"`
	Workspaces []string `json:"workspaces"` // exact ids, or ["*"] for all
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	var req createRequest
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := h.service.Create(r.Context(), CreateInput{
		Name:       req.Name,
		Workspaces: req.Workspaces,
	})
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

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	keys, err := h.service.List(r.Context())
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	if keys == nil {
		keys = []Key{}
	}
	rest.RespondJSON(w, http.StatusOK, keys)
}

func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	if err := h.service.Revoke(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, ErrNotFound) {
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

// Whoami answers "what can this credential do": the principal plus the
// accessible slice of the workspace registry. Any authenticated principal
// may call it.
func (h *Handler) Whoami(w http.ResponseWriter, r *http.Request) {
	p := auth.PrincipalFromContext(r.Context())
	if p == nil {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	registry, err := h.registry.List(r.Context())
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	accessible := make([]map[string]string, 0, len(registry))
	for _, ws := range registry {
		if p.CanAccess(ws.ID) {
			name := ws.Name
			if name == "" {
				name = ws.ID
			}
			accessible = append(accessible, map[string]string{"id": ws.ID, "name": name})
		}
	}
	resp := map[string]any{
		"root":       p.IsRoot(),
		"workspaces": accessible,
	}
	if !p.IsRoot() {
		resp["key"] = map[string]any{
			"id":             p.KeyID,
			"name":           p.Name,
			"all_workspaces": p.AllWorkspaces,
			"workspaces":     p.WorkspaceList(),
		}
	}
	rest.RespondJSON(w, http.StatusOK, resp)
}
