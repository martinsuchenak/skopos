package cmd

import "testing"

func TestReportCmdExists(t *testing.T) {
	cmd := reportCmd()
	if cmd == nil {
		t.Fatal("reportCmd should not return nil")
	}
	if cmd.Name != "report" {
		t.Fatalf("expected report command, got %q", cmd.Name)
	}
}
