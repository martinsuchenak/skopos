package integrations

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// Handler serves /api/webhooks and GET /api/events.
type Handler struct{ service *Service }

func NewHandler(s *Service) *Handler { return &Handler{service: s} }

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if g, ok := isGone(err); ok {
		rest.RespondJSON(w, http.StatusGone, map[string]any{"error": g.Error(), "oldest_seq": g.Oldest})
		return
	}
	switch {
	case errors.Is(err, ErrInvalidInput):
		rest.RespondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNoKey):
		rest.RespondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrNotFound):
		rest.RespondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, auth.ErrApproverRequired), errors.Is(err, auth.ErrRootRequired), errors.Is(err, auth.ErrOutOfScope):
		rest.RespondError(w, http.StatusForbidden, err.Error())
	default:
		rest.InternalError(w, err)
	}
}

// Create handles POST /api/webhooks.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in Input
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	in.Via = rest.Via(r)
	out, err := h.service.Create(r.Context(), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusCreated, out)
}

// List handles GET /api/webhooks.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	out, err := h.service.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, out)
}

// Get handles GET /api/webhooks/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	out, err := h.service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, out)
}

// Update handles PATCH /api/webhooks/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var in Input
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	in.Via = rest.Via(r)
	out, err := h.service.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, out)
}

// Delete handles DELETE /api/webhooks/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.service.Delete(r.Context(), r.PathValue("id"), rest.Via(r)); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Rotate handles POST /api/webhooks/{id}/rotate.
func (h *Handler) Rotate(w http.ResponseWriter, r *http.Request) {
	out, err := h.service.Rotate(r.Context(), r.PathValue("id"), rest.Via(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, out)
}

// Test handles POST /api/webhooks/{id}/test.
func (h *Handler) Test(w http.ResponseWriter, r *http.Request) {
	out, err := h.service.Test(r.Context(), r.PathValue("id"), rest.Via(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusAccepted, out)
}

// Deliveries handles GET /api/webhooks/{id}/deliveries?state=&limit=.
func (h *Handler) Deliveries(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := h.service.Deliveries(r.Context(), r.PathValue("id"), r.URL.Query().Get("state"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, out)
}

// Redeliver handles POST /api/webhooks/{id}/redeliver ({delivery_id} or
// {since}).
func (h *Handler) Redeliver(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DeliveryID string    `json:"delivery_id"`
		Since      time.Time `json:"since"`
	}
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	n, err := h.service.Redeliver(r.Context(), r.PathValue("id"), in.DeliveryID, in.Since, rest.Via(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, map[string]int64{"redelivered": n})
}

// Feed handles GET /api/events?after=&types=&workspace_id=&limit=&wait=.
func (h *Handler) Feed(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	q := FeedQuery{Types: parseTypes(qs.Get("types")), WorkspaceID: rest.QueryAlias(r, "workspace_id", "workspace")}
	var err error
	if v := qs.Get("after"); v != "" {
		if q.After, err = strconv.ParseInt(v, 10, 64); err != nil || q.After < 0 {
			rest.RespondError(w, http.StatusBadRequest, "after must be a sequence number")
			return
		}
	}
	q.Limit, _ = strconv.Atoi(qs.Get("limit"))
	if v := qs.Get("wait"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 0 {
			rest.RespondError(w, http.StatusBadRequest, "wait is seconds (at most 25)")
			return
		}
		q.Wait = time.Duration(secs) * time.Second
	}
	page, err := h.service.Feed(r.Context(), q)
	if err != nil {
		h.fail(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, page)
}
