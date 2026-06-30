package plans

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

type Handler struct {
	service *Service
	apiKey  string
}

func NewHandler(service *Service, apiKey string) *Handler {
	return &Handler{service: service, apiKey: apiKey}
}

func (h *Handler) authorized(r *http.Request) bool {
	return auth.Authorize(r, h.apiKey)
}

func (h *Handler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var input CreatePlanInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	plan, err := h.service.CreatePlan(r.Context(), input)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusCreated, plan)
}

func (h *Handler) ListPlans(w http.ResponseWriter, r *http.Request) {
	workspace := r.URL.Query().Get("workspace")
	branch := r.URL.Query().Get("branch")
	plans, err := h.service.ListPlans(r.Context(), workspace, branch)
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	if plans == nil {
		plans = []Plan{}
	}
	rest.RespondJSON(w, http.StatusOK, plans)
}

func (h *Handler) GetPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	plan, err := h.service.GetPlan(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, plan)
}

func (h *Handler) UpdatePlan(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	var input UpdatePlanInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.UpdatePlan(r.Context(), id, input); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
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

func (h *Handler) DeletePlan(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if err := h.service.DeletePlan(r.Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) AddItem(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	planID := r.PathValue("id")
	var input CreateItemInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	item, err := h.service.AddItem(r.Context(), planID, input)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	rest.RespondJSON(w, http.StatusCreated, item)
}

func (h *Handler) UpdateItem(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	planID := r.PathValue("id")
	itemID := r.PathValue("item_id")
	var input UpdateItemInput
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := h.service.UpdateItem(r.Context(), planID, itemID, input); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
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
	planID := r.PathValue("id")
	itemID := r.PathValue("item_id")
	if err := h.service.DeleteItem(r.Context(), planID, itemID); err != nil {
		if errors.Is(err, ErrNotFound) {
			rest.RespondError(w, http.StatusNotFound, err.Error())
			return
		}
		rest.InternalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) AddDependency(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	planID := r.PathValue("id")
	itemID := r.PathValue("item_id")
	var input struct {
		DependsOnID string `json:"depends_on_item_id"`
	}
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.AddDependency(r.Context(), planID, itemID, input.DependsOnID); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrCycleDetected):
			rest.RespondError(w, http.StatusConflict, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	item, err := h.service.GetPlan(r.Context(), planID)
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	for _, it := range item.Items {
		if it.ID == itemID {
			rest.RespondJSON(w, http.StatusOK, it)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) RemoveDependency(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	planID := r.PathValue("id")
	itemID := r.PathValue("item_id")
	dependsOnID := r.PathValue("depends_on_id")
	if err := h.service.RemoveDependency(r.Context(), planID, itemID, dependsOnID); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
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

func (h *Handler) AddPlanDependency(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	planID := r.PathValue("id")
	var input struct {
		DependsOnPlanID string `json:"depends_on_plan_id"`
	}
	if err := rest.DecodeJSON(w, r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.service.AddPlanDependency(r.Context(), planID, input.DependsOnPlanID); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrCycleDetected):
			rest.RespondError(w, http.StatusConflict, err.Error())
		default:
			rest.InternalError(w, err)
		}
		return
	}
	plan, err := h.service.GetPlan(r.Context(), planID)
	if err != nil {
		rest.InternalError(w, err)
		return
	}
	rest.RespondJSON(w, http.StatusOK, plan)
}

func (h *Handler) RemovePlanDependency(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	planID := r.PathValue("id")
	dependsOnID := r.PathValue("depends_on_id")
	if err := h.service.RemovePlanDependency(r.Context(), planID, dependsOnID); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
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
