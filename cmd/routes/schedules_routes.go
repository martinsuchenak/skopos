package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/schedules"
)

// RegisterScheduleRoutes mounts the schedules: an item template started on
// a rhythm (registered from serve.go).
func RegisterScheduleRoutes(mux *http.ServeMux, h *schedules.Handler) {
	mux.HandleFunc("GET /api/schedules", h.List)
	mux.HandleFunc("POST /api/schedules", h.Create)
	mux.HandleFunc("GET /api/schedules/{id}", h.Get)
	mux.HandleFunc("PUT /api/schedules/{id}", h.Update)
	mux.HandleFunc("DELETE /api/schedules/{id}", h.Delete)
	mux.HandleFunc("POST /api/schedules/{id}/run", h.Run)
	mux.HandleFunc("GET /api/schedules/{id}/firings", h.Firings)
}
