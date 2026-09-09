package routes

import (
	"html/template"
	"io/fs"
	"net/http"
	"runtime"

	"github.com/martinsuchenak/skopos/build"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	appweb "github.com/martinsuchenak/skopos/web"
)

var registrations []func(*http.ServeMux, *status.Handler)
var blackboardRegistrations []func(*http.ServeMux, *blackboard.Handler)
var plansRegistrations []func(*http.ServeMux, *plans.Handler)
var workspacesRegistrations []func(*http.ServeMux, *workspaces.Handler)

// RegisterStatus registers status/session routes. (Named for symmetry with
// RegisterBlackboard/RegisterPlans/RegisterWorkspaces.)
func RegisterStatus(fn func(*http.ServeMux, *status.Handler)) {
	registrations = append(registrations, fn)
}

func RegisterBlackboard(fn func(*http.ServeMux, *blackboard.Handler)) {
	blackboardRegistrations = append(blackboardRegistrations, fn)
}

func RegisterPlans(fn func(*http.ServeMux, *plans.Handler)) {
	plansRegistrations = append(plansRegistrations, fn)
}

func RegisterWorkspaces(fn func(*http.ServeMux, *workspaces.Handler)) {
	workspacesRegistrations = append(workspacesRegistrations, fn)
}

func RegisterRoutes(mux *http.ServeMux, statusHandler *status.Handler, blackboardHandler *blackboard.Handler, plansHandler *plans.Handler, workspacesHandler *workspaces.Handler) {
	mux.HandleFunc("GET /health", healthHandler)
	registerWebRoutes(mux)

	for _, fn := range registrations {
		fn(mux, statusHandler)
	}

	for _, fn := range blackboardRegistrations {
		fn(mux, blackboardHandler)
	}

	for _, fn := range plansRegistrations {
		fn(mux, plansHandler)
	}

	for _, fn := range workspacesRegistrations {
		fn(mux, workspacesHandler)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	rest.RespondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// MetricsHandler serves basic runtime metrics (goroutines, allocation). It is
// mounted in cmd.serve behind the API-key middleware so it is not exposed when
// auth is enabled.
func MetricsHandler(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	rest.RespondJSON(w, http.StatusOK, map[string]interface{}{
		"goroutines": runtime.NumGoroutine(),
		"alloc_mb":   m.Alloc / 1024 / 1024,
	})
}

func registerWebRoutes(mux *http.ServeMux) {
	templates := template.Must(template.ParseFS(appweb.TemplateFiles, "templates/base.html"))
	staticFS, err := fs.Sub(appweb.StaticFiles, "dist")
	if err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// The dashboard renders agent-authored content (via escaped text
		// interpolation only). CSP is the backstop keeping a future HTML-rendering
		// change from becoming stored XSS: scripts/styles/statics are same-origin,
		// data: for the favicon, and framing is denied (the UI has destructive
		// buttons — clickjacking). Notes: 'unsafe-eval' is required by Alpine's
		// expression evaluator (its CSP build would need a full template rewrite);
		// 'unsafe-inline' for styles only (Alpine's :style writes style attributes).
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		version := build.Version
		if build.Date != "unknown" {
			version += " · " + build.Date
		}
		templates.ExecuteTemplate(w, "base.html", map[string]any{"Title": "Dashboard", "Version": version})
	})
}
