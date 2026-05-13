package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/blackboard"
)

func init() {
	RegisterBlackboard(registerBlackboardRoutes)
}

func registerBlackboardRoutes(mux *http.ServeMux, h *blackboard.Handler) {
	mux.HandleFunc("POST /api/blackboard/entries", h.WriteEntry)
	mux.HandleFunc("GET /api/blackboard/entries", h.ReadBundle)
	mux.HandleFunc("PATCH /api/blackboard/entries/{id}/promote", h.Promote)
	mux.HandleFunc("DELETE /api/blackboard/entries/{id}", h.Delete)
}
