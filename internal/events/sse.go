package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// StreamHandler serves an SSE feed of hub events. Mount at GET /api/events/stream.
// It is a read endpoint and follows the same (open) access as the other GET /api list endpoints.
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

		ch, unsub := hub.Subscribe()
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

// Middleware wraps next and publishes an event to hub on successful mutating
// requests (POST/PATCH/PUT/DELETE with a 2xx response), inferring the event
// type from the request path. Reads and failures publish nothing.
func Middleware(hub *Hub, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if isMutation(r.Method) && rec.status >= 200 && rec.status < 300 {
			hub.Publish(Event{Type: typeForPath(r.URL.Path)})
		}
	})
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

func typeForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/blackboard"):
		return "blackboard"
	case strings.HasPrefix(path, "/api/plans"):
		return "plans"
	case strings.HasPrefix(path, "/api/workspaces"):
		return "workspaces"
	case strings.HasPrefix(path, "/api/sessions"), strings.HasPrefix(path, "/api/reports"):
		return "sessions"
	default:
		return "change"
	}
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
