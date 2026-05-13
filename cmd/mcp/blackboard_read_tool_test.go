package mcp

import "testing"

func TestBlackboardReadToolRegistered(t *testing.T) {
	if len(blackboardToolRegistrations) < 2 {
		t.Fatalf("expected at least 2 blackboard tool registrations (write + read), got %d", len(blackboardToolRegistrations))
	}
}
