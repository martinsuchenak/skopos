package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/paularlott/logger"
)

// StreamHandler serves an SSE feed of hub events. Mount at GET /api/events/stream.
// It is a read endpoint and follows the same access rules as the other GET /api
// endpoints: it requires the API key when one is configured.
func StreamHandler(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Clear the server's write deadline for this long-lived stream.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		streamCtx := r.Context()
		var ch <-chan Event
		var unsub func()
		if p := auth.PrincipalFromContext(streamCtx); p != nil && !p.Root {
			// Keyed subscription: scope-filtered and terminated when the
			// key is revoked (hub.DropKey closes the channel).
			ch, unsub = hub.SubscribeKeyed(p.KeyID, p.CanAccess)
		} else {
			ch, unsub = hub.Subscribe()
		}
		defer unsub()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				data, _ := json.Marshal(ev)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
				flusher.Flush()
			case <-ticker.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}
}

// Middleware wraps next for request logging. Event publishing lives in the
// service layer, where the mutation's workspace is authoritative — inferring
// it from requests (body peeks, path parsing, JSON-RPC probing) proved
// fragile and shipped broken once; the services that mutate data now call
// their Publisher directly.
func Middleware(log logger.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if log != nil {
			log.Debug("http request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start).String())
		}
	})
}
// statusRecorder captures the response status. It proxies Flush so streaming
// handlers (SSE) still work when wrapped.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.NewResponseController (SetWriteDeadline etc.) reach the real
// connection through this wrapper — required so the SSE handler can clear the
// server's WriteTimeout for the long-lived stream.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
