package usage

import (
	"strings"
	"testing"
)

// A stored record keeps to its bounds: names, models, sizes; a sum for
// reading does not.
func TestBounds(t *testing.T) {
	u := &Usage{}
	u.Add(&Usage{Counts: map[string]int64{"input_tokens": 5, "Bad Name": 1, "zero": 0, "neg": -3},
		Models: map[string]map[string]int64{" padded ": {"input_tokens": 1}, strings.Repeat("m", MaxModelName+1): {"input_tokens": 1},
			"ok\x00": {"input_tokens": 1}, "good": {"input_tokens": 2, "x": -1}}})
	if len(u.Counts) != 1 || u.Counts["input_tokens"] != 5 || len(u.Models) != 2 || u.Models["good"]["input_tokens"] != 2 ||
		len(u.Models["good"]) != 1 || u.Models["padded"]["input_tokens"] != 1 {
		t.Fatalf("only valid names and positive counts (a model name trimmed): %+v", u)
	}
	for i := range MaxModels + 3 {
		u.Add(&Usage{Models: map[string]map[string]int64{"model-" + string(rune('a'+i)): {"input_tokens": 1}}})
	}
	if len(u.Models) != MaxModels {
		t.Fatalf("models bounded: %d", len(u.Models))
	}
	big := &Usage{}
	var many []*Usage
	for i := range MaxCounts + 5 {
		k := "k" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		big.Count(k, 1)
		many = append(many, &Usage{Counts: map[string]int64{k: 1}})
	}
	if len(big.Counts) != MaxCounts {
		t.Fatalf("a stored record is bounded: %d", len(big.Counts))
	}
	if s := Sum(many...); len(s.Counts) != MaxCounts+5 {
		t.Fatalf("a sum is not: %d", len(s.Counts))
	}
	if Sum(nil, &Usage{}) != nil {
		t.Fatal("a sum of nothing is nil")
	}
}

// A runner that does not count per model has its tokens filed under the
// one model; one that does keeps its own breakdown.
func TestForModelAndTokens(t *testing.T) {
	u := &Usage{Counts: map[string]int64{"input_tokens": 1, "cache_read_tokens": 10, "cache_write_tokens": 100, "output_tokens": 7, "turns": 2}}
	u.ForModel("sonnet")
	if u.Models["sonnet"]["input_tokens"] != 1 || u.Models["sonnet"]["turns"] != 0 {
		t.Fatalf("token counts under the model: %v", u.Models)
	}
	u.ForModel("opus")
	if len(u.Models) != 1 || u.ModelNames()[0] != "sonnet" {
		t.Fatalf("a breakdown stays: %v", u.Models)
	}
	if in, out := u.Tokens(); in != 111 || out != 7 {
		t.Fatalf("tokens: %d %d", in, out)
	}
	for n, want := range map[int64]string{980: "980", 1234: "1.2k", 56_400: "56k", 4_100_000: "4.1M"} {
		if got := Compact(n); got != want {
			t.Errorf("Compact(%d) = %q, want %q", n, got, want)
		}
	}
}
