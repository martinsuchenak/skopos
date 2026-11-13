package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/fleetfile"
	"github.com/martinsuchenak/skopos/internal/secretbox"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// RegisterConfigRoutes mounts the workspace config and runner definition
// endpoints (registered from serve.go, like the pipeline's).
func RegisterConfigRoutes(mux *http.ServeMux, h *wsconfig.Handler, f *fleetfile.Handler, k *secretbox.Handler) {
	// MCP servers for runs: definitions, and their secrets' values (set,
	// never read back).
	mux.HandleFunc("GET /api/fleet/mcp-servers", h.ListMCPServers)
	mux.HandleFunc("PUT /api/fleet/mcp-servers/{name}", h.PutMCPServer)
	mux.HandleFunc("DELETE /api/fleet/mcp-servers/{name}", h.DeleteMCPServer)
	mux.HandleFunc("PUT /api/fleet/mcp-servers/{name}/secrets/{secret}", h.PutMCPSecret)
	mux.HandleFunc("DELETE /api/fleet/mcp-servers/{name}/secrets/{secret}", h.DeleteMCPSecret)
	// The server's secret key: what it sealed, and sealing it all again
	// under the current key.
	mux.HandleFunc("GET /api/fleet/secret-key", k.Status)
	mux.HandleFunc("POST /api/fleet/secret-key/rotate", k.Rotate)
	// Everything as one document: export, and apply (or plan) a file.
	mux.HandleFunc("GET /api/config/export", f.Export)
	mux.HandleFunc("POST /api/config/apply", f.Apply)
	mux.HandleFunc("GET /api/workspaces/{id}/config", h.GetWorkspace)
	mux.HandleFunc("PUT /api/workspaces/{id}/config", h.PutWorkspace)
	mux.HandleFunc("DELETE /api/workspaces/{id}/config", h.DeleteWorkspace)
	// What a save would be told, field by field, writing nothing.
	mux.HandleFunc("POST /api/workspaces/{id}/config/check", h.CheckWorkspace)
	mux.HandleFunc("POST /api/groups/{id}/config/check", h.CheckGroup)
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
	// Wrapper definitions: one per wrapper name, attached to runs by a
	// stage block's rules.
	mux.HandleFunc("GET /api/fleet/wrappers", h.ListWrappers)
	mux.HandleFunc("PUT /api/fleet/wrappers/{name}", h.PutWrapper)
	mux.HandleFunc("DELETE /api/fleet/wrappers/{name}", h.DeleteWrapper)
}
