package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/sample"
)

func init() {
	Register(registerSampleRoutes)
}

func registerSampleRoutes(mux *http.ServeMux) {
	sampleHandler := sample.NewHandler(sample.NewService(sample.NewStorage()))
	mux.HandleFunc("GET /api/samples", sampleHandler.List)
	mux.HandleFunc("POST /api/samples", sampleHandler.Create)
	mux.HandleFunc("GET /api/samples/{id}", sampleHandler.Get)
}
