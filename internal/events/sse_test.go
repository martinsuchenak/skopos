package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMiddlewarePublishesOnSuccess(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()

	mw := Middleware(h, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/reports", nil)
	mw.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case ev := <-ch:
		if ev.Type != "sessions" {
			t.Errorf("got %q, want sessions", ev.Type)
		}
	default:
		t.Fatal("expected an event after a 2xx POST")
	}
}

func TestMiddlewareSkipsReadsAndFailures(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()

	mw := Middleware(h, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	mw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/reports", nil))
	mw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/sessions", nil))

	select {
	case ev := <-ch:
		t.Fatalf("did not expect an event, got %v", ev)
	default:
	}
}

func TestTypeForPath(t *testing.T) {
	cases := map[string]string{
		"/api/reports":           "sessions",
		"/api/sessions/abc":      "sessions",
		"/api/blackboard/entries": "blackboard",
		"/api/plans/x/items":    "plans",
		"/api/workspaces":        "workspaces",
		"/health":                "change",
	}
	for p, want := range cases {
		if got := typeForPath(p); got != want {
			t.Errorf("typeForPath(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestStreamHandlerEmitsEvents(t *testing.T) {
	h := NewHub()
	handler := StreamHandler(h)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond) // allow the handler to subscribe
	h.Publish(Event{Type: "plans"})
	time.Sleep(30 * time.Millisecond) // allow the event to be written
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, ": connected") {
		t.Errorf("expected initial connected comment, got: %s", body)
	}
	if !strings.Contains(body, "event: plans") {
		t.Errorf("expected 'event: plans' in stream, got: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected text/event-stream content-type, got %q", ct)
	}
}
