package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/workspaces"
)

func init() {
	RegisterWorkspaces(registerWorkspaceRoutes)
}

func registerWorkspaceRoutes(mux *http.ServeMux, h *workspaces.Handler) {
	mux.HandleFunc("POST /api/workspaces", h.Create)
	mux.HandleFunc("GET /api/workspaces", h.List)
	mux.HandleFunc("DELETE /api/workspaces/{id}", h.Delete)
}
