package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/apikeys"
)

var keysRegistrations []func(*http.ServeMux, *apikeys.Handler)

// RegisterKeys registers API-key routes; the handler may be nil (feature
// disabled), in which case registrations skip themselves.
func RegisterKeys(fn func(*http.ServeMux, *apikeys.Handler)) {
	keysRegistrations = append(keysRegistrations, fn)
}

func init() {
	RegisterKeys(registerKeyRoutes)
}

func registerKeyRoutes(mux *http.ServeMux, h *apikeys.Handler) {
	if h == nil {
		return
	}
	mux.HandleFunc("GET /api/whoami", h.Whoami)
	mux.HandleFunc("POST /api/keys", h.Create)
	mux.HandleFunc("GET /api/keys", h.List)
	mux.HandleFunc("DELETE /api/keys/{id}", h.Revoke)
}
