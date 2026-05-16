package mcp

import "testing"

func TestPlanReadToolRegistered(t *testing.T) {
	if len(plansToolRegistrations) < 2 {
		t.Fatalf("expected at least 2 plans tool registrations, got %d", len(plansToolRegistrations))
	}
}
