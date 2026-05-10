package mcp

import "testing"

func TestSampleToolRegistered(t *testing.T) {
	if len(toolRegistrations) == 0 {
		t.Fatal("toolRegistrations should not be empty (sample tool registers via init())")
	}
}
