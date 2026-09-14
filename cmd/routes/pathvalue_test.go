package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Verifies that PathValue is visible to middleware wrapping the mux AFTER
// next.ServeHTTP has routed the request (the events middleware relies on it
// for workspace attribution).
func TestPathValueVisibleAfterRouting(t *testing.T) {
	var seen string
	inner := http.NewServeMux()
	inner.HandleFunc("POST /api/codeindex/{workspace}/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
	})
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r)
		seen = r.PathValue("workspace")
	})
	req := httptest.NewRequest("POST", "/api/codeindex/ws-x/refresh", nil)
	wrapped.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "ws-x" {
		t.Fatalf("PathValue not visible after routing: %q", seen)
	}
}
