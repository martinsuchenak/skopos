package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/codeindex"
)

func init() {
	RegisterCodeIndex(registerCodeIndexRoutes)
}

func registerCodeIndexRoutes(mux *http.ServeMux, h *codeindex.Handler) {
	if h == nil {
		return
	}
	mux.HandleFunc("POST /api/codeindex/{workspace}/manifest", h.Manifest)
	mux.HandleFunc("POST /api/codeindex/{workspace}/blobs", h.Blobs)
	mux.HandleFunc("POST /api/codeindex/{workspace}/commit", h.Commit)
	mux.HandleFunc("GET /api/codeindex/{workspace}/status", h.Status)
	mux.HandleFunc("GET /api/codeindex/{workspace}/search", h.Search)
	mux.HandleFunc("GET /api/codeindex/{workspace}/symbol", h.Symbol)
	mux.HandleFunc("GET /api/codeindex/{workspace}/outline", h.Outline)
	mux.HandleFunc("GET /api/codeindex/{workspace}/callers", h.Callers)
	mux.HandleFunc("GET /api/codeindex/{workspace}/callees", h.Callees)
	mux.HandleFunc("GET /api/codeindex/{workspace}/impact", h.Impact)
	mux.HandleFunc("GET /api/codeindex/{workspace}/dead", h.Dead)
	mux.HandleFunc("GET /api/codeindex/{workspace}/cycles", h.Cycles)
	mux.HandleFunc("GET /api/codeindex/{workspace}/call-tree", h.CallTree)
	mux.HandleFunc("GET /api/codeindex/{workspace}/branch-diff", h.BranchDiff)
	mux.HandleFunc("DELETE /api/codeindex/{workspace}/branch/{branch}", h.DropBranch)
	mux.HandleFunc("DELETE /api/codeindex/{workspace}", h.DropWorkspace)
	mux.HandleFunc("POST /api/codeindex/{workspace}/refresh", h.RefreshStart)
	mux.HandleFunc("GET /api/codeindex/{workspace}/refresh", h.RefreshStatus)
}
