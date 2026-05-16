package mcp

import "testing"

func TestPlanAddItemToolRegistered(t *testing.T) {
	if len(plansToolRegistrations) < 3 {
		t.Fatalf("expected at least 3 plans tool registrations, got %d", len(plansToolRegistrations))
	}
}
