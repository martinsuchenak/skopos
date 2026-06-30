package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
)

func TestNewMCPHandlerBuildsServer(t *testing.T) {
	handler := NewMCPHandler(&status.Service{}, &blackboard.Service{}, &plans.Service{})
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	w := httptest.NewRecorder()
	// Handler must be invokable without panicking on a bare request.
	handler.ServeHTTP(w, req)
}

func TestToolRegistrationsNotEmpty(t *testing.T) {
	if len(toolRegistrations) == 0 {
		t.Fatal("toolRegistrations should not be empty")
	}
	if len(blackboardToolRegistrations) == 0 {
		t.Fatal("blackboardToolRegistrations should not be empty")
	}
	if len(plansToolRegistrations) == 0 {
		t.Fatal("plansToolRegistrations should not be empty")
	}
}
