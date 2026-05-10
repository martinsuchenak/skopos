package routes

import (
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"runtime"

	"github.com/martinsuchenak/skopos/internal/status"
	appweb "github.com/martinsuchenak/skopos/web"
)

var registrations []func(*http.ServeMux, *status.Handler)

func Register(fn func(*http.ServeMux, *status.Handler)) {
	registrations = append(registrations, fn)
}

func RegisterRoutes(mux *http.ServeMux, statusHandler *status.Handler) {
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /metrics", metricsHandler)
	registerWebRoutes(mux)

	for _, fn := range registrations {
		fn(mux, statusHandler)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	json.NewEncoder(w).Encode(map[string]interface{}{
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		templates.ExecuteTemplate(w, "base.html", map[string]any{"Title": "Dashboard"})
	})
}
