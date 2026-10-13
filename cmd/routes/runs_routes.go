package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/runs"
)

var runsRegistrations []func(*http.ServeMux, *runs.Handler)

// RegisterRuns registers run-queue routes; the handler may be nil (feature
// disabled), in which case registrations skip themselves.
func RegisterRuns(fn func(*http.ServeMux, *runs.Handler)) {
	runsRegistrations = append(runsRegistrations, fn)
}

func init() {
	RegisterRuns(registerRunsRoutes)
}

func registerRunsRoutes(mux *http.ServeMux, h *runs.Handler) {
	if h == nil {
		return
	}
	mux.HandleFunc("POST /api/runs/claim", h.Claim)
	mux.HandleFunc("POST /api/runs/{id}/heartbeat", h.Heartbeat)
	mux.HandleFunc("POST /api/runs/{id}/report", h.Report)
	mux.HandleFunc("POST /api/runs/{id}/pause", h.Pause)
	mux.HandleFunc("POST /api/runs/{id}/resume", h.Resume)
	mux.HandleFunc("POST /api/runs/{id}/cancel", h.Cancel)
	mux.HandleFunc("GET /api/runs/{id}", h.Get)
	mux.HandleFunc("GET /api/runs", h.List)
}
