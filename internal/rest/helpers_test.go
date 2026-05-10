package rest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRespondJSON(t *testing.T) {
	w := httptest.NewRecorder()
	RespondJSON(w, http.StatusOK, map[string]string{"key": "value"})

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

func TestRespondError(t *testing.T) {
	w := httptest.NewRecorder()
	RespondError(w, http.StatusBadRequest, "bad input")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}
