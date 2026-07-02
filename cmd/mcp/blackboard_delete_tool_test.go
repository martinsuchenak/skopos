package mcp

import "testing"

func TestBlackboardDeleteToolRegistered(t *testing.T) {
	if len(blackboardToolRegistrations) < 2 {
		t.Fatalf("expected at least 2 blackboard tools (read/write/delete), got %d", len(blackboardToolRegistrations))
	}
}
