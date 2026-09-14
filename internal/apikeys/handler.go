package apikeys

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/workspaces"
)

type Handler struct {
	service  *Service
	registry *workspaces.Service
	publish  events.Publisher
}

func NewHandler(service *Service, registry *workspaces.Service) *Handler {
	return &Handler{service: service, registry: registry}
}

// SetPublisher installs the event bus; key lifecycle publishes an
// unattributed change event (the surface is root-only, so delivery is
// root-only under fail-closed filtering).
func (h *Handler) SetPublisher(p events.Publisher) { h.publish = p }

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
	if h.publish != nil {
		h.publish.Publish(events.Event{Type: events.TypeChange})
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

type updateRequest struct {
	Name       *string   `json:"name"`
	Workspaces *[]string `json:"workspaces"`
}

// Update handles PATCH /api/keys/{id}: partial edit of name and/or scope.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	var req updateRequest
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	input := UpdateInput{Name: req.Name}
	if req.Workspaces != nil {
		input.Workspaces = *req.Workspaces
	}
	key, err := h.service.Update(r.Context(), r.PathValue("id"), input)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, key)
}

func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	// ?hard=true removes the key row entirely (old/revoked-key cleanup);
	// the default is a soft revoke that keeps the audit trail.
	if r.URL.Query().Get("hard") == "true" {
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
		if h.publish != nil {
			h.publish.Publish(events.Event{Type: events.TypeChange})
		}
		w.WriteHeader(http.StatusNoContent)
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
	if h.publish != nil {
		h.publish.Publish(events.Event{Type: events.TypeChange})
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
