package events

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paularlott/logger"
)

// The middleware is logging-only now: publishing moved to the service layer
// where the mutation's workspace is authoritative (third pentest round,
// vuln-0004 remediation). These tests pin that no events leak through the
// HTTP layer regardless of method or status.
func TestMiddlewarePublishesNothing(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe()
	defer unsub()

	mw := Middleware(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	mw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/reports", nil))
	mw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/api/plans/x", nil))

	select {
	case ev := <-ch:
		t.Fatalf("middleware must not publish; got %v", ev)
	default:
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

// TestServerShutdownClosesStreams mirrors the cmd.serve wiring — an HTTP server
// with RegisterOnShutdown(hub.Close) must be able to shut down promptly while an
// SSE client is connected, because closing the hub ends the stream handler.
func TestServerShutdownClosesStreams(t *testing.T) {
	h := NewHub()
	ts := httptest.NewServer(StreamHandler(h))
	ts.Config.RegisterOnShutdown(h.Close)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The server's Shutdown only returns once the stream handler exits; without
	// the hub close it would block until the shutdown context deadline.
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- ts.Config.Shutdown(context.Background())
	}()

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown blocked on the open SSE stream — hub close was not registered or did not end it")
	}
}

// warnLog records warn-level messages and their key/values.
type warnLog struct {
	logger.Logger
	mu    sync.Mutex
	warns []map[string]any
}

func (l *warnLog) Debug(string, ...any) {}
func (l *warnLog) Warn(msg string, kv ...any) {
	m := map[string]any{"msg": msg}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	l.mu.Lock()
	l.warns = append(l.warns, m)
	l.mu.Unlock()
}

// A request past the threshold is logged at warn with the pool's state; a
// fast one and a stream are not.
func TestMiddlewareLogsSlowRequests(t *testing.T) {
	old := slowRequest
	slowRequest = 20 * time.Millisecond
	t.Cleanup(func() { slowRequest = old })

	log := &warnLog{}
	stats := sql.DBStats{MaxOpenConnections: 4}
	h := MiddlewareWithDBStats(log, func() sql.DBStats { return stats }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			stats.InUse, stats.WaitCount, stats.WaitDuration = 4, 3, 1500*time.Millisecond
			time.Sleep(30 * time.Millisecond)
			w.WriteHeader(http.StatusCreated)
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			time.Sleep(30 * time.Millisecond)
		}
	}))
	for _, p := range []string{"/fast", "/stream", "/slow"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, p+"?q=1", nil))
	}
	if len(log.warns) != 1 {
		t.Fatalf("want one warning (the slow request), got %v", log.warns)
	}
	w := log.warns[0]
	for k, want := range map[string]any{"msg": "slow http request", "path": "/slow", "status": http.StatusCreated,
		"db_in_use": 4, "db_max_open": 4, "db_pool_waits": int64(3), "db_pool_wait": "1.5s"} {
		if w[k] != want {
			t.Errorf("%s = %v (%T), want %v", k, w[k], w[k], want)
		}
	}
}
