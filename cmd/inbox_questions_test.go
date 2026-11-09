package cmd

import "testing"

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
