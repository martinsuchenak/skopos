package codeindex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
	"github.com/martinsuchenak/skopos/internal/rest"
)

type Handler struct {
	service    *Service
	apiKey     string
	registerWS func(id string)
	refresher  *Refresher
	embeddings *EmbeddingManager
}

func NewHandler(service *Service, apiKey string) *Handler {
	return &Handler{service: service, apiKey: apiKey}
}

// SetWorkspaceRegistrar installs a callback invoked when a commit names a
// workspace the registry has not seen (keeps pushed workspaces persistent).
func (h *Handler) SetWorkspaceRegistrar(fn func(id string)) { h.registerWS = fn }

// SetRefresher enables server-side indexing endpoints.
func (h *Handler) SetRefresher(r *Refresher) { h.refresher = r }

// SetEmbeddingManager enables asynchronous semantic embeddings.
func (h *Handler) SetEmbeddingManager(m *EmbeddingManager) { h.embeddings = m }

// SemanticSearcher exposes semantic search to the handler when configured.
func (h *Handler) semanticSearcher() Embedder {
	if h.embeddings == nil {
		return nil
	}
	return h.embeddings.embedder
}

func (h *Handler) maybeRegisterWorkspace(ws string) {
	if h.registerWS != nil {
		h.registerWS(ws)
	}
}

func (h *Handler) authorized(r *http.Request) bool { return auth.Authorize(r, h.apiKey) }

func (h *Handler) workspace(r *http.Request) string {
	return strings.ToLower(strings.TrimSpace(r.PathValue("workspace")))
}

func (h *Handler) requireWorkspace(w http.ResponseWriter, r *http.Request) (string, bool) {
	ws := h.workspace(r)
	if ws == "" {
		rest.RespondError(w, http.StatusBadRequest, "workspace path parameter is required")
		return "", false
	}
	return ws, true
}

// Manifest handles POST /api/codeindex/{workspace}/manifest: the client sends
// its file hashes, the server replies with the ones it needs.
func (h *Handler) Manifest(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	var req struct {
		Files []FileEntry `json:"files"`
	}
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	hashes := make([]string, 0, len(req.Files))
	for _, f := range req.Files {
		hashes = append(hashes, f.Hash)
	}
	missing, err := h.service.Store().HasBlobs(ws, hashes)
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

// Blobs handles POST /api/codeindex/{workspace}/blobs: an NDJSON stream of
// parsed-file payloads (parse.FileResult JSON), stored idempotently.
func (h *Handler) Blobs(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		rest.RespondError(w, http.StatusUnsupportedMediaType, "content type must be application/x-ndjson")
		return
	}
	// Blobs arrive as a stream (potentially large): cap at 256 MiB, well
	// above the 1 MiB JSON cap that suits ordinary API bodies.
	r.Body = http.MaxBytesReader(w, r.Body, 256<<20)

	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	stored, skipped := 0, 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var res parse.FileResult
		if err := json.Unmarshal(line, &res); err != nil || res.Hash == "" {
			skipped++
			continue
		}
		if err := h.service.Store().AddBlob(ws, &res); err != nil {
			rest.InternalError(w, err)
			return
		}
		stored++
	}
	if err := scanner.Err(); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "malformed ndjson body")
		return
	}
	rest.RespondJSON(w, http.StatusOK, map[string]int{"stored": stored, "skipped": skipped})
}

// Commit handles POST /api/codeindex/{workspace}/commit: atomically point a
// branch at a file set.
func (h *Handler) Commit(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	var req struct {
		Branch  string      `json:"branch"`
		HeadSHA string      `json:"head_sha"`
		Source  string      `json:"source"`
		Files   []FileEntry `json:"files"`
	}
	if err := rest.DecodeJSON(w, r, &req); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		rest.RespondError(w, http.StatusBadRequest, "branch is required")
		return
	}
	if len(req.Files) == 0 {
		rest.RespondError(w, http.StatusBadRequest, "files must not be empty")
		return
	}
	if err := h.service.Store().Commit(ws, req.Branch, req.HeadSHA, req.Source, req.Files); err != nil {
		rest.InternalError(w, err)
		return
	}
	h.maybeRegisterWorkspace(ws)
	if h.embeddings != nil {
		h.embeddings.Enqueue(ws)
	}
	rest.RespondJSON(w, http.StatusOK, map[string]any{
		"workspace": ws, "branch": req.Branch, "files": len(req.Files),
	})
}

// Status handles GET /api/codeindex/{workspace}/status.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	status, err := h.service.Status(r.Context(), ws)
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	if status == nil {
		status = []BranchStatus{}
	}
	rest.RespondJSON(w, http.StatusOK, status)
}

// Search handles GET /api/codeindex/{workspace}/search.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	var res *SearchResults
	var err error
	if r.URL.Query().Get("semantic") == "true" {
		res, err = h.service.SemanticSearch(r.Context(), ws, r.URL.Query().Get("branch"), r.URL.Query().Get("q"), queryLimit(r), h.semanticSearcher())
	} else {
		res, err = h.service.Search(r.Context(), ws, r.URL.Query().Get("branch"), r.URL.Query().Get("q"), queryLimit(r))
	}
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

// Symbol handles GET /api/codeindex/{workspace}/symbol.
func (h *Handler) Symbol(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	res, err := h.service.Symbol(r.Context(), ws, r.URL.Query().Get("branch"), r.URL.Query().Get("name"))
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

// Outline handles GET /api/codeindex/{workspace}/outline.
func (h *Handler) Outline(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	res, err := h.service.Outline(r.Context(), ws, r.URL.Query().Get("branch"), r.URL.Query().Get("path"))
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

// Callers handles GET /api/codeindex/{workspace}/callers.
func (h *Handler) Callers(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	res, err := h.service.Callers(r.Context(), ws, r.URL.Query().Get("branch"), r.URL.Query().Get("name"), queryLimit(r))
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

// Callees handles GET /api/codeindex/{workspace}/callees.
func (h *Handler) Callees(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	res, err := h.service.Callees(r.Context(), ws, r.URL.Query().Get("branch"), r.URL.Query().Get("name"), queryLimit(r))
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

// Impact handles GET /api/codeindex/{workspace}/impact.
func (h *Handler) Impact(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	depth := 0
	if d := r.URL.Query().Get("depth"); d != "" {
		var n int
		if _, err := fmt.Sscanf(d, "%d", &n); err == nil {
			depth = n
		}
	}
	res, err := h.service.Impact(r.Context(), ws, r.URL.Query().Get("branch"), r.URL.Query().Get("name"), depth)
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

// DropWorkspace handles DELETE /api/codeindex/{workspace}: tear down the
// whole workspace index (index DB + vectors in any backend).
func (h *Handler) DropWorkspace(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	if err := h.service.DropWorkspace(r.Context(), ws); err != nil {
		h.respondServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DropBranch handles DELETE /api/codeindex/{workspace}/branch/{branch}.
func (h *Handler) DropBranch(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	if err := h.service.DropBranch(r.Context(), ws, r.PathValue("branch")); err != nil {
		h.respondServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) respondServiceError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrInvalidInput) {
		rest.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	rest.InternalError(w, err)
}

func queryLimit(r *http.Request) int {
	if s := r.URL.Query().Get("limit"); s != "" {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
			return n
		}
	}
	return 0
}

// RefreshStart handles POST /api/codeindex/{workspace}/refresh: kick an
// asynchronous server-side clone/pull + rebuild from the workspace's git_url.
func (h *Handler) RefreshStart(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	if h.refresher == nil {
		rest.RespondError(w, http.StatusServiceUnavailable, "server-side indexing is not configured")
		return
	}
	var req struct {
		Branch string `json:"branch"`
	}
	if r.ContentLength > 0 {
		if err := rest.DecodeJSON(w, r, &req); err != nil {
			rest.RespondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	if err := h.refresher.Start(r.Context(), ws, req.Branch); err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusAccepted, map[string]any{"workspace": ws, "refreshing": true})
}

// RefreshStatus handles GET /api/codeindex/{workspace}/refresh.
func (h *Handler) RefreshStatus(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	if h.refresher == nil {
		rest.RespondError(w, http.StatusServiceUnavailable, "server-side indexing is not configured")
		return
	}
	rest.RespondJSON(w, http.StatusOK, h.refresher.State(ws))
}

// Dead handles GET /api/codeindex/{workspace}/dead.
func (h *Handler) Dead(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	res, err := h.service.Dead(r.Context(), ws, r.URL.Query().Get("branch"), queryLimit(r))
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

// Cycles handles GET /api/codeindex/{workspace}/cycles.
func (h *Handler) Cycles(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	res, err := h.service.Cycles(r.Context(), ws, r.URL.Query().Get("branch"))
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

// CallTree handles GET /api/codeindex/{workspace}/call-tree.
func (h *Handler) CallTree(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	depth := 0
	if d := r.URL.Query().Get("depth"); d != "" {
		var n int
		if _, err := fmt.Sscanf(d, "%d", &n); err == nil {
			depth = n
		}
	}
	res, err := h.service.CallTree(r.Context(), ws, r.URL.Query().Get("branch"), r.URL.Query().Get("name"), depth)
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}

// BranchDiff handles GET /api/codeindex/{workspace}/branch-diff.
func (h *Handler) BranchDiff(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ws, ok := h.requireWorkspace(w, r)
	if !ok {
		return
	}
	res, err := h.service.BranchDiff(r.Context(), ws, r.URL.Query().Get("branch"))
	if err != nil {
		h.respondServiceError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, res)
}
