package routes

import (
	"net/http"
	"testing"
)

func TestRegisterSampleRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerSampleRoutes(mux)
}
