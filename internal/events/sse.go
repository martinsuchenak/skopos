package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
			principal := p
			ch, unsub = hub.SubscribeFiltered(principal.CanAccess)
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

// Middleware wraps next and publishes an event to hub on successful mutating
// requests (POST/PATCH/PUT/DELETE with a 2xx response), inferring the event
// type from the request path. Reads and failures publish nothing.
func Middleware(hub *Hub, log logger.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if isMutation(r.Method) && rec.status >= 200 && rec.status < 300 {
			hub.Publish(Event{Type: typeForPath(r.URL.Path), Workspace: workspaceOf(r)})
		}
		if log != nil {
			log.Debug("http request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start).String())
		}
	})
}

// workspaceOf derives a mutation's workspace for subscriber filtering:
// the codeindex path parameter, the workspace query parameter, or the JSON
// body's workspace_id (peeked non-destructively, bounded). Empty means
// unattributed — such events carry no data and always pass filters.
func workspaceOf(r *http.Request) string {
	if rest := r.PathValue("workspace"); rest != "" {
		return strings.ToLower(rest)
	}
	if ws := r.URL.Query().Get("workspace"); ws != "" {
		return ws
	}
	if ws := r.URL.Query().Get("workspace_id"); ws != "" {
		return ws
	}
	if r.Body == nil {
		return ""
	}
	// Peek a bounded prefix and splice the unread remainder back: replacing
	// the body wholesale would truncate requests larger than the peek
	// (blob uploads, MCP calls). Parsing is skipped for truncated peeks —
	// a workspace_id that far into a body is not worth the risk.
	const peekLimit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, peekLimit))
	if err != nil {
		// The original body may still hold unread bytes; splice what we got.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		return ""
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	if len(body) == peekLimit {
		return "" // truncated peek — do not parse a partial document
	}
	var probe struct {
		WorkspaceID string `json:"workspace_id"`
		Workspace   string `json:"workspace"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	if probe.WorkspaceID != "" {
		return probe.WorkspaceID
	}
	return probe.Workspace
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

// Unwrap lets http.NewResponseController (SetWriteDeadline etc.) reach the real
// connection through this wrapper — required so the SSE handler can clear the
// server's WriteTimeout for the long-lived stream.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
