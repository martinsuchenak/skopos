package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Authorize reports whether the request is permitted to perform a write.
// It accepts the shared API key via either the X-API-Key header or an
// Authorization: Bearer <key> header (the form MCP clients typically send).
// When apiKey is empty, authentication is disabled and all requests are
// allowed (used for local development). Comparison is constant-time.
func Authorize(r *http.Request, apiKey string) bool {
	if apiKey == "" {
		return true
	}
	candidate := r.Header.Get("X-API-Key")
	if candidate == "" {
		if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
			candidate = strings.TrimPrefix(v, "Bearer ")
		}
	}
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(apiKey)) == 1
}

// APIKeyMiddleware returns middleware that requires the given API key (via the
// X-API-Key header or Authorization: Bearer). When apiKey is empty, the
// middleware is a no-op (authentication disabled). CORS preflight (OPTIONS)
// requests are always allowed through.
func APIKeyMiddleware(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodOptions && !Authorize(r, apiKey) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
