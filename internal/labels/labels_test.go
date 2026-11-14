package labels

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	got, err := Normalize([]string{" Stock ", "plant-2", "stock", "", "a.b_c"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.b_c", "plant-2", "stock"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, err := Normalize([]string{"", "  "}); err != nil || got != nil {
		t.Fatalf("blanks: got %v, %v", got, err)
	}
	for _, bad := range []string{"-stock", "has space", "a,b", "é", strings.Repeat("x", 33), "a/b"} {
		if _, err := Normalize([]string{bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: got %v, want ErrInvalid", bad, err)
		}
	}
	nine := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	if _, err := Normalize(nine); !errors.Is(err, ErrInvalid) {
		t.Errorf("nine labels: got %v, want ErrInvalid", err)
	}
	if _, err := Normalize(nine[:Max]); err != nil {
		t.Errorf("eight labels: %v", err)
	}
}

func TestCoversAndMissing(t *testing.T) {
	cases := []struct {
		has, needs []string
		covers     bool
		missing    []string
	}{
		{nil, nil, true, nil},
		{[]string{"stock"}, nil, true, nil},
		{nil, []string{"stock"}, false, []string{"stock"}},
		{[]string{"plant-2", "stock"}, []string{"stock"}, true, nil},
		{[]string{"stock"}, []string{"plant-2", "stock"}, false, []string{"plant-2"}},
	}
	for _, c := range cases {
		if got := Covers(c.has, c.needs); got != c.covers {
			t.Errorf("Covers(%v, %v) = %v", c.has, c.needs, got)
		}
		if got := Missing(c.has, c.needs); !slices.Equal(got, c.missing) {
			t.Errorf("Missing(%v, %v) = %v", c.has, c.needs, got)
		}
	}
}

func TestStoredForm(t *testing.T) {
	if JSON(nil) != "" || Parse("") != nil || Parse("not json") != nil || Parse("[]") != nil {
		t.Fatal("no labels must be the empty string and read back as nil")
	}
	list := []string{"plant-2", "stock"}
	if got := Parse(JSON(list)); !slices.Equal(got, list) {
		t.Fatalf("round trip: %v", got)
	}
	if got, _ := Normalize(Split(" stock, plant-2 ,")); !slices.Equal(got, list) {
		t.Fatalf("Split: %v", got)
	}
	if list, set := Arg(" "); set || list != nil {
		t.Fatalf("Arg blank: %v %v", list, set)
	}
	if list, set := Arg("-"); !set || list == nil || len(list) != 0 {
		t.Fatalf("Arg clear: %v %v", list, set)
	}
	if list, set := Arg("stock,plant-2"); !set || len(list) != 2 {
		t.Fatalf("Arg list: %v %v", list, set)
	}
}
