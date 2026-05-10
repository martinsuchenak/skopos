package routes

import (
	"encoding/json"
	"net/http"
	"runtime"
)

var registrations []func(*http.ServeMux)

func Register(fn func(*http.ServeMux)) {
	registrations = append(registrations, fn)
}

func RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /metrics", metricsHandler)

	for _, fn := range registrations {
		fn(mux)
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
