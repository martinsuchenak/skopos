package dbtime

import (
	"sort"
	"testing"
	"time"
)

// Text order equals time order — including the cases RFC3339Nano gets
// wrong (trailing zeros trimmed, a whole second, a non-UTC offset) — and
// every string round-trips, legacy forms included.
func TestFixedWidthSortsAndParses(t *testing.T) {
	base := time.Date(2026, 10, 17, 10, 0, 5, 0, time.UTC)
	times := []time.Time{
		base,
		base.Add(100 * time.Millisecond),
		base.Add(120 * time.Millisecond),
		base.Add(123450 * time.Microsecond),
		base.Add(123456 * time.Microsecond),
		base.Add(500 * time.Millisecond),
		base.Add(time.Second),
		base.Add(2 * time.Second).In(time.FixedZone("CST", 8*3600)),
	}
	strs := make([]string, len(times))
	for i, tm := range times {
		strs[i] = Format(tm)
		if len(strs[i]) != Width {
			t.Fatalf("width %d: %q", len(strs[i]), strs[i])
		}
	}
	if !sort.StringsAreSorted(strs) {
		t.Fatalf("text order must equal time order: %v", strs)
	}
	for i, s := range strs {
		got, err := Parse(s)
		if err != nil || !got.Equal(times[i]) {
			t.Fatalf("round trip %q: %v %v", s, got, err)
		}
	}
	for _, legacy := range []string{"2026-10-17T10:00:05Z", "2026-10-17T10:00:05.5Z", "2026-10-17T18:00:05.123+08:00"} {
		if _, err := Parse(legacy); err != nil {
			t.Fatalf("legacy %q must parse: %v", legacy, err)
		}
	}
}
