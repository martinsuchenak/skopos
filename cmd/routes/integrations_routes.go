package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/integrations"
)

var integrationsRegistrations []func(*http.ServeMux, *integrations.Handler)

// RegisterIntegrations registers webhook and event-feed routes.
func RegisterIntegrations(fn func(*http.ServeMux, *integrations.Handler)) {
	integrationsRegistrations = append(integrationsRegistrations, fn)
}

// MountIntegrations mounts the registered integration routes on apiMux
// (nil handler = not served).
func MountIntegrations(apiMux *http.ServeMux, h *integrations.Handler) {
	if h == nil {
		return
	}
	for _, fn := range integrationsRegistrations {
		fn(apiMux, h)
	}
}

func init() {
	RegisterIntegrations(registerIntegrationRoutes)
}

// Webhook subscriptions and the event feed (tmp/integrations — design.md
// §3): management is root's, or an approver's for workspaces in scope.
func registerIntegrationRoutes(mux *http.ServeMux, h *integrations.Handler) {
	mux.HandleFunc("GET /api/events", h.Feed)
	mux.HandleFunc("POST /api/webhooks", h.Create)
	mux.HandleFunc("GET /api/webhooks", h.List)
	mux.HandleFunc("GET /api/webhooks/{id}", h.Get)
	mux.HandleFunc("PATCH /api/webhooks/{id}", h.Update)
	mux.HandleFunc("DELETE /api/webhooks/{id}", h.Delete)
	mux.HandleFunc("POST /api/webhooks/{id}/rotate", h.Rotate)
	mux.HandleFunc("POST /api/webhooks/{id}/test", h.Test)
	mux.HandleFunc("GET /api/webhooks/{id}/deliveries", h.Deliveries)
	mux.HandleFunc("POST /api/webhooks/{id}/redeliver", h.Redeliver)
}
