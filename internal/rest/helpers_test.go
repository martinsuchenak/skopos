package rest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	logslog "github.com/paularlott/logger/slog"
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
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control no-store, got %q", cc)
	}
}

func TestRespondJSONNil(t *testing.T) {
	w := httptest.NewRecorder()
	RespondJSON(w, http.StatusNoContent, nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("expected %d, got %d", http.StatusNoContent, w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("expected empty body for nil data, got %d bytes", w.Body.Len())
	}
}

func TestRespondError(t *testing.T) {
	w := httptest.NewRecorder()
	RespondError(w, http.StatusBadRequest, "bad input")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "bad input" {
		t.Errorf("expected error 'bad input', got %q", body["error"])
	}
}

func TestInternalErrorWithoutLogger(t *testing.T) {
	SetLogger(nil)
	w := httptest.NewRecorder()
	InternalError(w, errors.New("boom"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if body["error"] != "internal server error" {
		t.Errorf("expected generic message, got %q", body["error"])
	}
}

func TestInternalErrorWithLogger(t *testing.T) {
	SetLogger(logslog.New(logslog.Config{Level: "error", Writer: io.Discard}))
	w := httptest.NewRecorder()
	InternalError(w, errors.New("boom"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestDecodeJSONValid(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"key":"value"}`))
	var v map[string]string
	if err := DecodeJSON(w, r, &v); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v["key"] != "value" {
		t.Errorf("expected key=value, got %v", v)
	}
}

func TestDecodeJSONInvalid(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{bad json`))
	var v map[string]string
	if err := DecodeJSON(w, r, &v); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestDecodeJSONOversized(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 2<<20)))
	var v map[string]string
	if err := DecodeJSON(w, r, &v); err == nil {
		t.Error("expected error for body exceeding 1 MiB limit")
	}
}

func TestQueryAlias(t *testing.T) {
	r := httptest.NewRequest("GET", "/?workspace_id=ws-1&branch=main", nil)
	if got := QueryAlias(r, "workspace_id", "workspace"); got != "ws-1" {
		t.Errorf("canonical name: expected ws-1, got %q", got)
	}

	r = httptest.NewRequest("GET", "/?workspace=ws-legacy", nil)
	if got := QueryAlias(r, "workspace_id", "workspace"); got != "ws-legacy" {
		t.Errorf("legacy alias: expected ws-legacy, got %q", got)
	}

	r = httptest.NewRequest("GET", "/", nil)
	if got := QueryAlias(r, "workspace_id", "workspace"); got != "" {
		t.Errorf("no params: expected empty, got %q", got)
	}

	r = httptest.NewRequest("GET", "/?workspace_id=&workspace=ws-legacy", nil)
	if got := QueryAlias(r, "workspace_id", "workspace"); got != "ws-legacy" {
		t.Errorf("empty canonical falls through: expected ws-legacy, got %q", got)
	}
}

func TestBodyLimit(t *testing.T) {
	var readErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.Copy(io.Discard, r.Body)
	})
	h := BodyLimit(inner)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("hello")))
	if readErr != nil {
		t.Fatalf("small body should read cleanly, got %v", readErr)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 2<<20))))
	if readErr == nil {
		t.Fatal("expected read error for body exceeding 1 MiB limit")
	}
}
