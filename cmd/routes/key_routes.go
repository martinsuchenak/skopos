package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/apikeys"
)

var keysRegistrations []func(*http.ServeMux, *apikeys.Handler)

// RegisterKeys registers API-key routes; the handler may be nil (feature
// disabled), in which case registrations skip themselves.
func RegisterKeys(fn func(*http.ServeMux, *apikeys.Handler)) {
	keysRegistrations = append(keysRegistrations, fn)
}

func init() {
	RegisterKeys(registerKeyRoutes)
}

func registerKeyRoutes(mux *http.ServeMux, h *apikeys.Handler) {
	if h == nil {
		return
	}
	mux.HandleFunc("GET /api/whoami", h.Whoami)
	mux.HandleFunc("POST /api/keys", h.Create)
	mux.HandleFunc("GET /api/keys", h.List)
	mux.HandleFunc("GET /api/keys/who-can", h.WhoCan)
	mux.HandleFunc("DELETE /api/keys/{id}", h.Revoke)
	mux.HandleFunc("PATCH /api/keys/{id}", h.Update)

	// Workspace groups (docs/design/agent-pipeline.md §3). Root-only,
	// enforced in the handler and the service.
	mux.HandleFunc("POST /api/groups", h.CreateGroup)
	mux.HandleFunc("GET /api/groups", h.ListGroups)
	mux.HandleFunc("GET /api/groups/{id}", h.GetGroup)
	mux.HandleFunc("PATCH /api/groups/{id}", h.UpdateGroup)
	mux.HandleFunc("DELETE /api/groups/{id}", h.DeleteGroup)

	// Workspace-centric group allocation: which groups hold a workspace.
	mux.HandleFunc("GET /api/workspaces/{id}/groups", h.WorkspaceGroups)
	mux.HandleFunc("PUT /api/workspaces/{id}/groups", h.SetWorkspaceGroups)
}
