package plans

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testHandler(t *testing.T, apiKey string) *Handler {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable fk: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewHandler(NewService(NewStorage(sqlDB)), apiKey)
}

func TestHandlerCreatePlanRequiresAuth(t *testing.T) {
	h := testHandler(t, "secret")
	body := bytes.NewBufferString(`{"name":"Plan","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandlerCreateAndGetPlan(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{"name":"Auth refactor","branch_name":"feat-auth","author_agent_id":"agent-1"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", w.Code, w.Body.String())
	}

	var plan Plan
	if err := json.NewDecoder(w.Body).Decode(&plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if plan.ID == "" {
		t.Fatal("expected ID")
	}

	req2 := httptest.NewRequest("GET", "/api/plans/"+plan.ID, nil)
	req2.SetPathValue("id", plan.ID)
	w2 := httptest.NewRecorder()
	h.GetPlan(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", w2.Code)
	}
	var got Plan
	if err := json.NewDecoder(w2.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Auth refactor" {
		t.Errorf("expected name Auth refactor, got %q", got.Name)
	}
}

func TestHandlerListPlans(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{"name":"P1","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}

	req2 := httptest.NewRequest("GET", "/api/plans", nil)
	w2 := httptest.NewRecorder()
	h.ListPlans(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w2.Code)
	}
	var plans []Plan
	if err := json.NewDecoder(w2.Body).Decode(&plans); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
}

func TestHandlerAddItemAndPatchStatus(t *testing.T) {
	h := testHandler(t, "")

	body := bytes.NewBufferString(`{"name":"P","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	var plan Plan
	json.NewDecoder(w.Body).Decode(&plan)

	itemBody := bytes.NewBufferString(`{"title":"Fix auth bug"}`)
	req2 := httptest.NewRequest("POST", "/api/plans/"+plan.ID+"/items", itemBody)
	req2.SetPathValue("id", plan.ID)
	w2 := httptest.NewRecorder()
	h.AddItem(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("add item: expected 201, got %d body=%s", w2.Code, w2.Body.String())
	}
	var item Item
	json.NewDecoder(w2.Body).Decode(&item)

	patchBody := bytes.NewBufferString(`{"status":"done"}`)
	req3 := httptest.NewRequest("PATCH", "/api/plans/"+plan.ID+"/items/"+item.ID, patchBody)
	req3.SetPathValue("id", plan.ID)
	req3.SetPathValue("item_id", item.ID)
	w3 := httptest.NewRecorder()
	h.UpdateItem(w3, req3)
	if w3.Code != http.StatusNoContent {
		t.Fatalf("patch item: expected 204, got %d body=%s", w3.Code, w3.Body.String())
	}

	req4 := httptest.NewRequest("GET", "/api/plans/"+plan.ID, nil)
	req4.SetPathValue("id", plan.ID)
	w4 := httptest.NewRecorder()
	h.GetPlan(w4, req4)
	var updated Plan
	json.NewDecoder(w4.Body).Decode(&updated)
	if len(updated.Items) != 1 || updated.Items[0].Status != ItemDone {
		t.Errorf("expected item done, got %v", updated.Items)
	}
}

func TestHandlerDeletePlanCascades(t *testing.T) {
	h := testHandler(t, "")

	body := bytes.NewBufferString(`{"name":"P","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	var plan Plan
	json.NewDecoder(w.Body).Decode(&plan)

	req2 := httptest.NewRequest("DELETE", "/api/plans/"+plan.ID, nil)
	req2.SetPathValue("id", plan.ID)
	w2 := httptest.NewRecorder()
	h.DeletePlan(w2, req2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", w2.Code)
	}

	req3 := httptest.NewRequest("GET", "/api/plans/"+plan.ID, nil)
	req3.SetPathValue("id", plan.ID)
	w3 := httptest.NewRecorder()
	h.GetPlan(w3, req3)
	if w3.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w3.Code)
	}
}

func TestHandlerGetPlanNotFound(t *testing.T) {
	h := testHandler(t, "")
	req := httptest.NewRequest("GET", "/api/plans/missing", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	h.GetPlan(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandlerListPlansWorkspaceFilter(t *testing.T) {
	h := testHandler(t, "")

	body1 := bytes.NewBufferString(`{"name":"Global","author_agent_id":"a"}`)
	req1 := httptest.NewRequest("POST", "/api/plans", body1)
	w1 := httptest.NewRecorder()
	h.CreatePlan(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("create global: %d", w1.Code)
	}

	body2 := bytes.NewBufferString(`{"name":"WS Plan","author_agent_id":"a","workspace_id":"ws-1"}`)
	req2 := httptest.NewRequest("POST", "/api/plans", body2)
	w2 := httptest.NewRecorder()
	h.CreatePlan(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("create ws plan: %d", w2.Code)
	}

	req3 := httptest.NewRequest("GET", "/api/plans?workspace=ws-1", nil)
	w3 := httptest.NewRecorder()
	h.ListPlans(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w3.Code)
	}
	var plans []Plan
	if err := json.NewDecoder(w3.Body).Decode(&plans); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan (ws-1 only), got %d", len(plans))
	}
}

func TestHandlerCreatePlanWithWorkspaceID(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{"name":"WS Plan","author_agent_id":"a","workspace_id":"ws-42"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", w.Code)
	}
	var plan Plan
	if err := json.NewDecoder(w.Body).Decode(&plan); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if plan.WorkspaceID != "ws-42" {
		t.Errorf("expected workspace_id ws-42, got %q", plan.WorkspaceID)
	}
}

func TestHandlerAddDependencyAndAutoBlock(t *testing.T) {
	h := testHandler(t, "")

	body := bytes.NewBufferString(`{"name":"P","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	var plan Plan
	json.NewDecoder(w.Body).Decode(&plan)

	for _, title := range []string{"First", "Second"} {
		itemBody := bytes.NewBufferString(fmt.Sprintf(`{"title":"%s"}`, title))
		req := httptest.NewRequest("POST", "/api/plans/"+plan.ID+"/items", itemBody)
		req.SetPathValue("id", plan.ID)
		w := httptest.NewRecorder()
		h.AddItem(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("add %s: %d body=%s", title, w.Code, w.Body.String())
		}
	}

	planReq := httptest.NewRequest("GET", "/api/plans/"+plan.ID, nil)
	planReq.SetPathValue("id", plan.ID)
	planW := httptest.NewRecorder()
	h.GetPlan(planW, planReq)
	var fullPlan Plan
	json.NewDecoder(planW.Body).Decode(&fullPlan)

	depBody := bytes.NewBufferString(fmt.Sprintf(`{"depends_on_item_id":"%s"}`, fullPlan.Items[0].ID))
	depReq := httptest.NewRequest("POST", fmt.Sprintf("/api/plans/%s/items/%s/dependencies", plan.ID, fullPlan.Items[1].ID), depBody)
	depReq.SetPathValue("id", plan.ID)
	depReq.SetPathValue("item_id", fullPlan.Items[1].ID)
	depW := httptest.NewRecorder()
	h.AddDependency(depW, depReq)
	if depW.Code != http.StatusOK {
		t.Fatalf("add dep: expected 200, got %d body=%s", depW.Code, depW.Body.String())
	}

	var updatedItem Item
	json.NewDecoder(depW.Body).Decode(&updatedItem)
	if updatedItem.Status != ItemBlocked {
		t.Errorf("expected blocked, got %q", updatedItem.Status)
	}
}

func TestHandlerRemoveDependency(t *testing.T) {
	h := testHandler(t, "")

	body := bytes.NewBufferString(`{"name":"P","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	var plan Plan
	json.NewDecoder(w.Body).Decode(&plan)

	for _, title := range []string{"A", "B"} {
		itemBody := bytes.NewBufferString(fmt.Sprintf(`{"title":"%s"}`, title))
		req := httptest.NewRequest("POST", "/api/plans/"+plan.ID+"/items", itemBody)
		req.SetPathValue("id", plan.ID)
		w := httptest.NewRecorder()
		h.AddItem(w, req)
	}

	planReq := httptest.NewRequest("GET", "/api/plans/"+plan.ID, nil)
	planReq.SetPathValue("id", plan.ID)
	planW := httptest.NewRecorder()
	h.GetPlan(planW, planReq)
	var fullPlan Plan
	json.NewDecoder(planW.Body).Decode(&fullPlan)

	depBody := bytes.NewBufferString(fmt.Sprintf(`{"depends_on_item_id":"%s"}`, fullPlan.Items[0].ID))
	depReq := httptest.NewRequest("POST", fmt.Sprintf("/api/plans/%s/items/%s/dependencies", plan.ID, fullPlan.Items[1].ID), depBody)
	depReq.SetPathValue("id", plan.ID)
	depReq.SetPathValue("item_id", fullPlan.Items[1].ID)
	h.AddDependency(httptest.NewRecorder(), depReq)

	doneBody := bytes.NewBufferString(`{"status":"done"}`)
	doneReq := httptest.NewRequest("PATCH", fmt.Sprintf("/api/plans/%s/items/%s", plan.ID, fullPlan.Items[0].ID), doneBody)
	doneReq.SetPathValue("id", plan.ID)
	doneReq.SetPathValue("item_id", fullPlan.Items[0].ID)
	doneW := httptest.NewRecorder()
	h.UpdateItem(doneW, doneReq)
	if doneW.Code != http.StatusNoContent {
		t.Fatalf("done item: expected 204, got %d", doneW.Code)
	}

	remReq := httptest.NewRequest("DELETE", fmt.Sprintf("/api/plans/%s/items/%s/dependencies/%s", plan.ID, fullPlan.Items[1].ID, fullPlan.Items[0].ID), nil)
	remReq.SetPathValue("id", plan.ID)
	remReq.SetPathValue("item_id", fullPlan.Items[1].ID)
	remReq.SetPathValue("depends_on_id", fullPlan.Items[0].ID)
	remW := httptest.NewRecorder()
	h.RemoveDependency(remW, remReq)
	if remW.Code != http.StatusNoContent {
		t.Fatalf("remove dep: expected 204, got %d body=%s", remW.Code, remW.Body.String())
	}
}

func TestHandlerAddItemWithPosition(t *testing.T) {
	h := testHandler(t, "")

	body := bytes.NewBufferString(`{"name":"P","author_agent_id":"a"}`)
	req := httptest.NewRequest("POST", "/api/plans", body)
	w := httptest.NewRecorder()
	h.CreatePlan(w, req)
	var plan Plan
	json.NewDecoder(w.Body).Decode(&plan)

	itemBody := bytes.NewBufferString(`{"title":"First","position":0}`)
	req1 := httptest.NewRequest("POST", "/api/plans/"+plan.ID+"/items", itemBody)
	req1.SetPathValue("id", plan.ID)
	w1 := httptest.NewRecorder()
	h.AddItem(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("add first: %d body=%s", w1.Code, w1.Body.String())
	}

	insertBody := bytes.NewBufferString(`{"title":"Inserted","position":0}`)
	req2 := httptest.NewRequest("POST", "/api/plans/"+plan.ID+"/items", insertBody)
	req2.SetPathValue("id", plan.ID)
	w2 := httptest.NewRecorder()
	h.AddItem(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("add inserted: %d body=%s", w2.Code, w2.Body.String())
	}

	getReq := httptest.NewRequest("GET", "/api/plans/"+plan.ID, nil)
	getReq.SetPathValue("id", plan.ID)
	getW := httptest.NewRecorder()
	h.GetPlan(getW, getReq)
	var updated Plan
	json.NewDecoder(getW.Body).Decode(&updated)
	if len(updated.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(updated.Items))
	}
	if updated.Items[0].Title != "Inserted" {
		t.Errorf("expected first item Inserted, got %q", updated.Items[0].Title)
	}
}
