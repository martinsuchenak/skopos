package textcut

import "testing"

func TestTruncate(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"héllo", 2, "h"},               // é is two bytes: never split
		{"ééé", 3, "é"},                 // back off to the boundary
		{"\x80\x80\x80", 2, "\x80\x80"}, // no boundary: hard cut
	} {
		if got := Truncate(c.in, c.n); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
