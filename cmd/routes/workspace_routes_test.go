package routes

import (
	"net/http"
	"testing"

	"github.com/martinsuchenak/skopos/internal/workspaces"
)

func TestRegisterWorkspaceRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerWorkspaceRoutes(mux, workspaces.NewHandler(workspaces.NewService(nil), ""))
	// Smoke: the handler is wired (nil service is fine; we don't invoke it here).
	_ = mux
}
