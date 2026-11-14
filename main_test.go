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

// A refused command line points at --help; any other failure does not.
func TestPrintErrorPointsAtHelp(t *testing.T) {
	for _, msg := range []string{"missing required argument: id", "too many arguments", "unknown flag: --wroker"} {
		var buf bytes.Buffer
		printErrorTo(&buf, errors.New(msg))
		if want := "skopos: " + msg + "\nAdd --help to the command to see its arguments and options\n"; buf.String() != want {
			t.Errorf("%q: got %q", msg, buf.String())
		}
	}
}
