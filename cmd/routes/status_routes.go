package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/status"
)

func init() {
	Register(registerStatusRoutes)
}

func registerStatusRoutes(mux *http.ServeMux, statusHandler *status.Handler) {
	mux.HandleFunc("POST /api/reports", statusHandler.Report)
	mux.HandleFunc("GET /api/sessions", statusHandler.ListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", statusHandler.GetSession)
	mux.HandleFunc("GET /api/sessions/{id}/events", statusHandler.ListEvents)
}
