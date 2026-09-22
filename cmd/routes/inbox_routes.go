package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/inbox"
)

func init() {
	RegisterInbox(registerInboxRoutes)
}

func registerInboxRoutes(mux *http.ServeMux, h *inbox.Handler) {
	mux.HandleFunc("POST /api/inbox", h.CreateItem)
	mux.HandleFunc("GET /api/inbox", h.ListItems)
	mux.HandleFunc("POST /api/inbox/reorder", h.Reorder)
	mux.HandleFunc("GET /api/inbox/{id}", h.GetItem)
	mux.HandleFunc("PATCH /api/inbox/{id}", h.UpdateItem)
	mux.HandleFunc("POST /api/inbox/{id}/claim", h.Claim)
	mux.HandleFunc("POST /api/inbox/{id}/convert", h.Convert)
	mux.HandleFunc("POST /api/inbox/{id}/discard", h.Discard)
	mux.HandleFunc("POST /api/inbox/{id}/restore", h.Restore)
	mux.HandleFunc("POST /api/inbox/{id}/complete", h.Complete)
	mux.HandleFunc("POST /api/inbox/{id}/reopen", h.Reopen)
	mux.HandleFunc("DELETE /api/inbox/{id}", h.DeleteItem)
	mux.HandleFunc("DELETE /api/inbox", h.Purge)
}
