package inbox

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

type Handler struct {
	service *Service
	authn   *auth.Authenticator
}

func NewHandler(service *Service, authn *auth.Authenticator) *Handler {
	return &Handler{service: service, authn: authn}
}

func (h *Handler) authorized(r *http.Request) bool {
	return h.authn.Authenticate(r) != nil
}

func (h *Handler) CreateItem(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var input CreateInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	item, err := h.service.CreateItem(r.Context(), input)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusForbidden, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusCreated, item)
}

func (h *Handler) ListItems(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	workspace := rest.QueryAlias(r, "workspace_id", "workspace")
	status := r.URL.Query().Get("status")
	tag := r.URL.Query().Get("tag")
	query := r.URL.Query().Get("q")
	items, err := h.service.ListItems(r.Context(), workspace, status, tag, query)
	if err != nil {
		if errors.Is(err, auth.ErrOutOfScope) {
			rest.RespondError(w, http.StatusForbidden, err.Error())
			return
		}
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	if items == nil {
		items = []Item{}
	}
	rest.RespondJSON(w, http.StatusOK, items)
}

func (h *Handler) GetItem(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	item, err := h.service.GetItem(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, item)
}

func (h *Handler) UpdateItem(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	var input UpdateInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.UpdateItem(r.Context(), id, input); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, auth.ErrOutOfScope):
			// The only OutOfScope source on this path is the filing target
			// (the item itself was scope-quiet-checked) — actionable 403.
			rest.RespondError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrFrozen):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) Claim(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	var input struct {
		AgentID string `json:"agent_id"`
	}
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	item, err := h.service.Claim(r.Context(), id, input.AgentID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrClaimConflict):
			rest.RespondError(w, http.StatusConflict, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, item)
}

func (h *Handler) Convert(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	var input ConvertInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	item, err := h.service.Convert(r.Context(), id, input)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrAlreadyConverted):
			rest.RespondError(w, http.StatusConflict, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, item)
}

func (h *Handler) Discard(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if err := h.service.Discard(r.Context(), id); err != nil {
		switch {
			case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
				rest.RespondError(w, http.StatusNotFound, err.Error())
			case errors.Is(err, ErrInvalidInput):
				rest.RespondError(w, http.StatusBadRequest, err.Error())
			default:
				rest.InternalError(w, err)
			}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) DeleteItem(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if err := h.service.DeleteItem(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Reorder assigns explicit priorities 1..N to the posted id order (the
// board's drag-and-drop renumbering).
func (h *Handler) Reorder(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var input ReorderInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.Reorder(r.Context(), input); err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}


// Restore brings a discarded item back to open.
func (h *Handler) Restore(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if err := h.service.Restore(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Complete manually marks an item done (work that finished without a plan,
// or ahead of it) — the drag-to-Done lane target.
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if err := h.service.Complete(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Reopen brings a done item back to open (undo a wrong manual complete).
func (h *Handler) Reopen(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if err := h.service.Reopen(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, ErrNotFound) || errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Purge handles DELETE /api/inbox — the collection-level bulk delete:
// workspace_id required, optional status narrows to one lane. Responds 200
// with {"deleted":N}. Out-of-scope maps to an actionable 403
// (explicit-target semantics), not a 404: this is not a by-id oracle case.
func (h *Handler) Purge(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	workspaceID := rest.QueryAlias(r, "workspace_id", "workspace")
	status := Status(r.URL.Query().Get("status"))
	deleted, err := h.service.Purge(r.Context(), workspaceID, status)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, auth.ErrOutOfScope):
			rest.RespondError(w, http.StatusForbidden, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}
