package mcp

import "testing"

func TestReportStatusToolRegistered(t *testing.T) {
	if len(toolRegistrations) == 0 {
		t.Fatal("toolRegistrations should not be empty (report_status tool registers via init())")
	}
}
