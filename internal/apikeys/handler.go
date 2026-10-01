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
	Groups     []string `json:"groups"`     // group names or ids
	Approver   bool     `json:"approver"`
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
		Groups:     req.Groups,
		Approver:   req.Approver,
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
	Groups     *[]string `json:"groups"`
	Approver   *bool     `json:"approver"`
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
	input := UpdateInput{Name: req.Name, Approver: req.Approver}
	if req.Workspaces != nil {
		input.Workspaces = *req.Workspaces
	}
	if req.Groups != nil {
		input.Groups = *req.Groups
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
// may call it. Keys holding groups get the group list and a per-workspace
// "via" explanation (explicit, group:<name>, pattern:<group>:<pattern>).
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
	var resolution *ScopeResolution
	if !p.IsRoot() && p.KeyID != "" {
		res, err := h.service.ResolveScope(r.Context(), p.KeyID)
		switch {
		case err == nil:
			resolution = res
		case errors.Is(err, ErrNotFound):
			// The key row is gone (hard-deleted) but its principal still
			// resolves; answer from the principal alone.
		default:
			rest.InternalError(w, err)
			return
		}
	}
	accessible := make([]map[string]any, 0, len(registry))
	for _, ws := range registry {
		if p.CanAccess(ws.ID) {
			name := ws.Name
			if name == "" {
				name = ws.ID
			}
			entry := map[string]any{"id": ws.ID, "name": name}
			if resolution != nil {
				if via, ok := resolution.Via[ws.ID]; ok {
					entry["via"] = via
				}
			}
			accessible = append(accessible, entry)
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
			"approver":       p.Approver,
			"workspaces":     p.WorkspaceList(),
		}
		if resolution != nil {
			resp["groups"] = resolution.Groups
		}
	}
	rest.RespondJSON(w, http.StatusOK, resp)
}

// WhoCan handles GET /api/keys/who-can?workspace=<id>: the reverse view of
// scope — every active key that reaches the workspace, and how. Root-only.
func (h *Handler) WhoCan(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	reach, err := h.service.WhoCan(r.Context(), r.URL.Query().Get("workspace"))
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, auth.ErrRootRequired):
			rest.RespondError(w, http.StatusForbidden, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, reach)
}

// --- workspace groups (docs/design/agent-pipeline.md §3) ---

type groupRequest struct {
	Name        string   `json:"name"`
	Description *string  `json:"description"` // pointer: absent = unchanged on PATCH, empty string = clear
	Members     []string `json:"members"`
	Patterns    []string `json:"patterns"`
}

// CreateGroup handles POST /api/groups. Root-only.
func (h *Handler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	var req groupRequest
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	g, err := h.service.CreateGroup(r.Context(), GroupInput{
		Name:        req.Name,
		Description: deref(req.Description),
		Members:     req.Members,
		Patterns:    req.Patterns,
	})
	if err != nil {
		h.respondGroupError(w, err)
		return
	}
	if h.publish != nil {
		h.publish.Publish(events.Event{Type: events.TypeChange})
	}
	rest.RespondJSON(w, http.StatusCreated, g)
}

// ListGroups handles GET /api/groups. Root-only.
func (h *Handler) ListGroups(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	groups, err := h.service.ListGroups(r.Context())
	if err != nil {
		if errors.Is(err, auth.ErrRootRequired) {
			rest.RespondError(w, http.StatusForbidden, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, groups)
}

// GetGroup handles GET /api/groups/{id}. Root-only.
func (h *Handler) GetGroup(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	g, err := h.service.GetGroup(r.Context(), r.PathValue("id"))
	if err != nil {
		h.respondGroupError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, g)
}

// UpdateGroup handles PATCH /api/groups/{id}: nil lists leave members and
// patterns unchanged. Root-only. Membership changes terminate the SSE streams
// of every key holding the group.
func (h *Handler) UpdateGroup(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	var req groupRequest
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	input := GroupUpdateInput{}
	if req.Name != "" {
		input.Name = &req.Name
	}
	// Pointer semantics: an absent description leaves it unchanged, an
	// explicit empty string clears it.
	input.Description = req.Description
	if req.Members != nil {
		input.Members = req.Members
	}
	if req.Patterns != nil {
		input.Patterns = req.Patterns
	}
	g, err := h.service.UpdateGroup(r.Context(), r.PathValue("id"), input)
	if err != nil {
		h.respondGroupError(w, err)
		return
	}
	if h.publish != nil {
		h.publish.Publish(events.Event{Type: events.TypeChange})
	}
	rest.RespondJSON(w, http.StatusOK, g)
}

// DeleteGroup handles DELETE /api/groups/{id}. Root-only.
func (h *Handler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	if h.requireRoot(w, r) == nil {
		return
	}
	if err := h.service.DeleteGroup(r.Context(), r.PathValue("id")); err != nil {
		h.respondGroupError(w, err)
		return
	}
	if h.publish != nil {
		h.publish.Publish(events.Event{Type: events.TypeChange})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) respondGroupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		rest.RespondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrDuplicateName):
		rest.RespondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrNotFound):
		rest.RespondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, auth.ErrRootRequired):
		rest.RespondError(w, http.StatusForbidden, err.Error())
	default:
		rest.InternalError(w, err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
