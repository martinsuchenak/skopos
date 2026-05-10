package sample

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerList(t *testing.T) {
	h := NewHandler(NewService(NewStorage()))
	req := httptest.NewRequest("GET", "/api/samples", nil)
	w := httptest.NewRecorder()
	h.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}
