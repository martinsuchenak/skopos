package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIKeyMiddleware(t *testing.T) {
	handler := APIKeyMiddleware("test-key")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", "test-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestAPIKeyMiddlewareUnauthorized(t *testing.T) {
	handler := APIKeyMiddleware("test-key")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestAuthorizeBearerToken(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	if !Authorize(req, "test-key") {
		t.Error("bearer token should authorize")
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	if Authorize(req, "test-key") {
		t.Error("wrong bearer token should not authorize")
	}
}

func TestAuthorizeEmptyKeyAllowsAll(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	if !Authorize(req, "") {
		t.Error("empty api key should allow all requests")
	}
}
