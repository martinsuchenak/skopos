package cmd

import (
	"testing"
	"time"
)

func TestAnswerText(t *testing.T) {
	got, err := answerText([]string{"1=a", "2 = Redis, for now"}, "drop question 3")
	if err != nil || got != "1) a\n2) Redis, for now\ndrop question 3" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := answerText([]string{"1"}, ""); err == nil {
		t.Fatal("an answer needs n=value")
	}
	if _, err := answerText(nil, " "); err == nil {
		t.Fatal("nothing to answer")
	}
}

func TestLocalDay(t *testing.T) {
	loc := time.FixedZone("AWST", 8*3600)
	old := time.Local
	time.Local = loc
	defer func() { time.Local = old }()
	if got := localDay("2026-11-09", false); got != "2026-11-09T00:00:00+08:00" {
		t.Fatalf("from: %s", got)
	}
	if got := localDay("2026-11-09", true); got != "2026-11-10T00:00:00+08:00" {
		t.Fatalf("to: %s", got)
	}
	if got := localDay("2026-11-09T10:00:00Z", false); got != "2026-11-09T10:00:00Z" {
		t.Fatalf("a time passes through: %s", got)
	}
}
