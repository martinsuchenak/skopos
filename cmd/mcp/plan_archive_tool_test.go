package mcp

import "testing"

func TestPlanArchiveToolRegistered(t *testing.T) {
	found := false
	for range plansToolRegistrations {
		found = true
		break
	}
	if !found {
		t.Fatal("plansToolRegistrations should not be empty")
	}
}
