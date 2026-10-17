package routes

import (
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"net/http"
	"runtime"
	"strings"

	"github.com/martinsuchenak/skopos/build"
	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/runs"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	appweb "github.com/martinsuchenak/skopos/web"
)

var registrations []func(*http.ServeMux, *status.Handler)
var blackboardRegistrations []func(*http.ServeMux, *blackboard.Handler)
var plansRegistrations []func(*http.ServeMux, *plans.Handler)
var inboxRegistrations []func(*http.ServeMux, *inbox.Handler)
var workspacesRegistrations []func(*http.ServeMux, *workspaces.Handler)
var codeIndexRegistrations []func(*http.ServeMux, *codeindex.Handler)

// RegisterStatus registers status/session routes. (Named for symmetry with
// RegisterBlackboard/RegisterPlans/RegisterInbox/RegisterWorkspaces.)
func RegisterStatus(fn func(*http.ServeMux, *status.Handler)) {
	registrations = append(registrations, fn)
}

func RegisterBlackboard(fn func(*http.ServeMux, *blackboard.Handler)) {
	blackboardRegistrations = append(blackboardRegistrations, fn)
}

func RegisterPlans(fn func(*http.ServeMux, *plans.Handler)) {
	plansRegistrations = append(plansRegistrations, fn)
}

func RegisterInbox(fn func(*http.ServeMux, *inbox.Handler)) {
	inboxRegistrations = append(inboxRegistrations, fn)
}

func RegisterWorkspaces(fn func(*http.ServeMux, *workspaces.Handler)) {
	workspacesRegistrations = append(workspacesRegistrations, fn)
}

// RegisterCodeIndex registers code-index routes; the handler may be nil (the
// feature disabled), in which case registrations skip themselves.
func RegisterCodeIndex(fn func(*http.ServeMux, *codeindex.Handler)) {
	codeIndexRegistrations = append(codeIndexRegistrations, fn)
}

// RegisterRoutes mounts the public surface (dashboard, statics, health) on
// webMux and the authenticated API on apiMux. The caller wraps apiMux with
// auth (see cmd.serve: the principal must reach services via the request
// context, so authentication lives in middleware, not in each handler).
func RegisterRoutes(webMux, apiMux *http.ServeMux, statusHandler *status.Handler, blackboardHandler *blackboard.Handler, plansHandler *plans.Handler, inboxHandler *inbox.Handler, workspacesHandler *workspaces.Handler, codeIndexHandler *codeindex.Handler, keysHandler *apikeys.Handler, runsHandler *runs.Handler) {
	webMux.HandleFunc("GET /health", healthHandler)
	registerWebRoutes(webMux)

	for _, fn := range registrations {
		fn(apiMux, statusHandler)
	}

	for _, fn := range blackboardRegistrations {
		fn(apiMux, blackboardHandler)
	}

	for _, fn := range plansRegistrations {
		fn(apiMux, plansHandler)
	}

	for _, fn := range inboxRegistrations {
		fn(apiMux, inboxHandler)
	}

	for _, fn := range workspacesRegistrations {
		fn(apiMux, workspacesHandler)
	}

	for _, fn := range codeIndexRegistrations {
		fn(apiMux, codeIndexHandler)
	}

	for _, fn := range keysRegistrations {
		fn(apiMux, keysHandler)
	}

	for _, fn := range runsRegistrations {
		fn(apiMux, runsHandler)
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

// bundleHash fingerprints every file in the bundle — paths and contents, in
// WalkDir's lexical order — so any asset change yields a new version.
func bundleHash(fsys fs.FS) (string, error) {
	h := fnv.New64a()
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		h.Write([]byte(path))
		h.Write([]byte{0})
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// dashboardTemplates parses base.html with the shared partials
// (templates/partials.html: modal shell, checklist, badges). `dict` builds a
// partial's parameter map: {{template "modal-open" (dict "ID" "x" ...)}}.
func dashboardTemplates() *template.Template {
	return template.Must(template.New("base.html").Funcs(template.FuncMap{
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
	}).ParseFS(appweb.TemplateFiles, "templates/*.html"))
}

func registerWebRoutes(mux *http.ServeMux) {
	templates := dashboardTemplates()
	// assetsVersion derives the dashboard's cache-busting suffix from the
	// embedded bundle CONTENT: the asset URLs change exactly when the assets
	// do (every dev rebuild, every release upgrade), so a browser can never
	// run a cached app.js or style.css against markup from a newer binary —
	// the "hard reload fixes it" class of confusion. Every file counts: a
	// markup-only change ships new Tailwind utilities in style.css alone.
	// Falls back to the build version when the bundle cannot be read.
	assetsVersion := build.Version
	staticFS, err := fs.Sub(appweb.StaticFiles, "dist")
	if err == nil {
		if v, hashErr := bundleHash(staticFS); hashErr == nil {
			assetsVersion = v
		}
		// Serve files but never directory listings: the bundle contains only
		// named assets, and an index of dist/ leaks the asset inventory.
		files := http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))
		mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/") {
				http.NotFound(w, r)
				return
			}
			// URLs are content-versioned in the HTML (see assetsVersion);
			// a versioned URL always serves that exact content forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
		}))
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
		// buttons — clickjacking). Alpine runs its CSP build (@alpinejs/csp) so no
		// 'unsafe-eval' is needed; 'unsafe-inline' applies to styles only
		// (Alpine's :style writes style attributes).
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The HTML is tiny and references versioned assets; it must always be
		// revalidated so a new binary's asset URLs are picked up immediately.
		// no-store, not just no-cache: the HTML names the hashed assets of
		// ITS build — a cached page against a newer server serves stale JS
		// that half-breaks (found live: "data won't load until devtools").
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		version := build.Version
		if build.Date != "unknown" {
			version += " · " + build.Date
		}
		templates.ExecuteTemplate(w, "base.html", map[string]any{"Title": "Dashboard", "Version": version, "Assets": assetsVersion})
	})
}
