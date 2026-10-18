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
}
