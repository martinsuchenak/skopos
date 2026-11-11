package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/fleetfile"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// RegisterConfigRoutes mounts the workspace config and runner definition
// endpoints (registered from serve.go, like the pipeline's).
func RegisterConfigRoutes(mux *http.ServeMux, h *wsconfig.Handler, f *fleetfile.Handler) {
	// Everything as one document: export, and apply (or plan) a file.
	mux.HandleFunc("GET /api/config/export", f.Export)
	mux.HandleFunc("POST /api/config/apply", f.Apply)
	mux.HandleFunc("GET /api/workspaces/{id}/config", h.GetWorkspace)
	mux.HandleFunc("PUT /api/workspaces/{id}/config", h.PutWorkspace)
	mux.HandleFunc("DELETE /api/workspaces/{id}/config", h.DeleteWorkspace)
	// What applies to a workspace now, and what a revision id names (the
	// config a run was claimed with).
	mux.HandleFunc("GET /api/workspaces/{id}/config/effective", h.Effective)
	mux.HandleFunc("GET /api/workspaces/{id}/config/{rev}", h.At)
	mux.HandleFunc("GET /api/groups/{id}/config", h.GetGroup)
	mux.HandleFunc("PUT /api/groups/{id}/config", h.PutGroup)
	mux.HandleFunc("DELETE /api/groups/{id}/config", h.DeleteGroup)
	// Runner definitions: one per runner name, for every worker.
	mux.HandleFunc("GET /api/fleet/config", h.FleetConfig)
	mux.HandleFunc("GET /api/fleet/runners", h.ListRunners)
	mux.HandleFunc("PUT /api/fleet/runners/{name}", h.PutRunner)
	mux.HandleFunc("DELETE /api/fleet/runners/{name}", h.DeleteRunner)
}
