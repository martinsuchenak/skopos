package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Authorize reports whether the request is permitted to perform a write.
// It accepts the API key via the Authorization: Bearer <key> header.
// When apiKey is empty, authentication is disabled and all requests are
// allowed (used for local development). Comparison is constant-time.
func Authorize(r *http.Request, apiKey string) bool {
	if apiKey == "" {
		return true
	}
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(v, "Bearer ")), []byte(apiKey)) == 1
}

// APIKeyMiddleware returns middleware that requires the given API key via
// Authorization: Bearer. When apiKey is empty, the middleware is a no-op
// (authentication disabled). CORS preflight (OPTIONS) requests always pass.
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
