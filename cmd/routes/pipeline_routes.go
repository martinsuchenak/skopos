package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/pipeline"
)

// RegisterPipelineRoutes mounts the pipeline settings endpoints (registered
// from serve.go: the handler is optional for the shared RegisterRoutes).
func RegisterPipelineRoutes(mux *http.ServeMux, h *pipeline.Handler) {
	mux.HandleFunc("GET /api/workspaces/{id}/pipeline", h.GetWorkspace)
	mux.HandleFunc("PUT /api/workspaces/{id}/pipeline", h.PutWorkspace)
	mux.HandleFunc("GET /api/groups/{id}/pipeline", h.GetGroup)
	mux.HandleFunc("PUT /api/groups/{id}/pipeline", h.PutGroup)
	// Briefs held on the server: what a member with a focus is told.
	mux.HandleFunc("GET /api/workspaces/{id}/pipeline/briefs", h.ListWorkspaceBriefs)
	mux.HandleFunc("GET /api/workspaces/{id}/pipeline/briefs/{name}", h.GetWorkspaceBrief)
	mux.HandleFunc("PUT /api/workspaces/{id}/pipeline/briefs/{name}", h.PutWorkspaceBrief)
	mux.HandleFunc("DELETE /api/workspaces/{id}/pipeline/briefs/{name}", h.DeleteWorkspaceBrief)
	mux.HandleFunc("GET /api/groups/{id}/pipeline/briefs", h.ListGroupBriefs)
	mux.HandleFunc("GET /api/groups/{id}/pipeline/briefs/{name}", h.GetGroupBrief)
	mux.HandleFunc("PUT /api/groups/{id}/pipeline/briefs/{name}", h.PutGroupBrief)
	mux.HandleFunc("DELETE /api/groups/{id}/pipeline/briefs/{name}", h.DeleteGroupBrief)
	// Runner policy: which runners may work on which workspaces.
	mux.HandleFunc("GET /api/policy/fleet/{level}", h.GetFleetPolicy)
	mux.HandleFunc("PUT /api/policy/fleet/{level}", h.PutFleetPolicy)
	mux.HandleFunc("DELETE /api/policy/fleet/{level}", h.DeleteFleetPolicy)
	mux.HandleFunc("GET /api/workspaces/{id}/policy", h.GetWorkspacePolicy)
	mux.HandleFunc("PUT /api/workspaces/{id}/policy", h.PutWorkspacePolicy)
	mux.HandleFunc("DELETE /api/workspaces/{id}/policy", h.DeleteWorkspacePolicy)
	mux.HandleFunc("GET /api/workspaces/{id}/policy/effective", h.EffectivePolicy)
	mux.HandleFunc("GET /api/groups/{id}/policy", h.GetGroupPolicy)
	mux.HandleFunc("PUT /api/groups/{id}/policy", h.PutGroupPolicy)
	mux.HandleFunc("DELETE /api/groups/{id}/policy", h.DeleteGroupPolicy)
}
