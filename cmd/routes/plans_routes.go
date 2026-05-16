package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/plans"
)

func init() {
	RegisterPlans(registerPlansRoutes)
}

func registerPlansRoutes(mux *http.ServeMux, h *plans.Handler) {
	mux.HandleFunc("POST /api/plans", h.CreatePlan)
	mux.HandleFunc("GET /api/plans", h.ListPlans)
	mux.HandleFunc("GET /api/plans/{id}", h.GetPlan)
	mux.HandleFunc("PATCH /api/plans/{id}", h.UpdatePlan)
	mux.HandleFunc("DELETE /api/plans/{id}", h.DeletePlan)
	mux.HandleFunc("POST /api/plans/{id}/items", h.AddItem)
	mux.HandleFunc("PATCH /api/plans/{id}/items/{item_id}", h.UpdateItem)
	mux.HandleFunc("DELETE /api/plans/{id}/items/{item_id}", h.DeleteItem)
	mux.HandleFunc("POST /api/plans/{id}/items/{item_id}/dependencies", h.AddDependency)
	mux.HandleFunc("DELETE /api/plans/{id}/items/{item_id}/dependencies/{depends_on_id}", h.RemoveDependency)
	mux.HandleFunc("POST /api/plans/{id}/dependencies", h.AddPlanDependency)
	mux.HandleFunc("DELETE /api/plans/{id}/dependencies/{depends_on_id}", h.RemovePlanDependency)
}
