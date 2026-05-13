package mcp

import "testing"

func TestBlackboardWriteToolRegistered(t *testing.T) {
	if len(blackboardToolRegistrations) == 0 {
		t.Fatal("blackboardToolRegistrations should not be empty")
	}
}
