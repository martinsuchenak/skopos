package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPrintError(t *testing.T) {
	var buf bytes.Buffer
	printErrorTo(&buf, errors.New("branch feat is not indexed"))
	got := buf.String()
	if got != "skopos: branch feat is not indexed\n" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "ERR") || strings.Contains(got, "AWST") {
		t.Fatalf("log noise leaked into CLI error: %q", got)
	}
	_ = os.Stderr // keep os referenced if main uses it
}
