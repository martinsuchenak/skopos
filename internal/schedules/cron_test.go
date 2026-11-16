package schedules

import (
	"testing"
	"time"
)

func TestRhythmNext(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, london)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct{ expr, after, want string }{
		{"monthly", "2026-03-04 09:00", "2026-04-01 06:00"},
		{"monthly", "2026-03-01 05:59", "2026-03-01 06:00"},
		{"0 6 1 * *", "2026-03-01 06:00", "2026-04-01 06:00"},
		{"weekly", "2026-03-04 09:00", "2026-03-09 06:00"}, // a Wednesday → Monday
		{"30 9 * * mon-fri", "2026-03-06 10:00", "2026-03-09 09:30"},
		{"*/15 * * * *", "2026-03-04 09:07", "2026-03-04 09:15"},
		{"0 0 29 feb *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 6 1,15 * *", "2026-03-02 00:00", "2026-03-15 06:00"},
		{"0 6 13 * 5", "2026-03-01 00:00", "2026-03-06 06:00"}, // the 13th OR a Friday
		{"0 1 * * 0", "2026-03-21 12:00", "2026-03-22 01:00"},  // Sunday as 0
		{"0 1 * * 7", "2026-03-21 12:00", "2026-03-22 01:00"},  // and as 7
		// 01:00 does not exist the night the clocks go forward: that day
		// is passed over.
		{"0 1 * * 0", "2026-03-28 12:00", "2026-04-05 01:00"},
	} {
		r, err := ParseRhythm(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		got := r.Next(at(tc.after), london)
		if want := at(tc.want); !got.Equal(want) {
			t.Errorf("%s after %s: %s, want %s", tc.expr, tc.after, got, want)
		}
	}
	if r, _ := ParseRhythm("0 0 31 2 *"); !r.Next(at("2026-01-01 00:00"), london).IsZero() {
		t.Error("31 February never comes")
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "0 24 * * *", "0 0 0 * *", "0 0 * 13 *", "0 0 * * 8", "*/0 * * * *", "a * * * *", "5-1 * * * *"} {
		if _, err := ParseRhythm(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
