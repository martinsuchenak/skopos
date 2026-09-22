package routes

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// The dashboard must reference the static assets through content-versioned
// URLs (see assetsVersion in api_routes.go): a cached app.js from an older
// binary then can never run against new markup — the "hard reload fixes
// it" failure mode.
func TestDashboardAssetVersioning(t *testing.T) {
	mux := http.NewServeMux()
	registerWebRoutes(mux)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, asset := range []string{"theme.js", "style.css", "app.js"} {
		re := regexp.MustCompile(`/static/` + asset + `\?v=[A-Za-z0-9._-]+`)
		if !re.MatchString(body) {
			t.Fatalf("asset %s is not content-versioned in the dashboard HTML", asset)
		}
	}
	if w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("dashboard HTML must be revalidated every load, got %q", w.Header().Get("Cache-Control"))
	}

	// Versioned asset URLs may be cached forever (the URL changes with the
	// content). The header is set before the file server answers, so this
	// holds even in an unbuilt checkout where app.js itself 404s.
	r = httptest.NewRequest(http.MethodGet, "/static/app.js?v=whatever", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("static assets must be immutable, got %q", cc)
	}
}
