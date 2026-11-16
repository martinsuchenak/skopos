package schedules

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // timezones in a container without a zoneinfo database
)

// A rhythm is a five-field cron expression — minute hour day-of-month month
// day-of-week — or one of the names below. Fields take `*`, a number, a
// range `a-b`, a step `*/n` or `a-b/n`, and lists of those; months and days
// of the week take their English three-letter names; Sunday is 0 or 7. When
// both day fields are restricted, a day matching either counts (as cron
// does). A time that does not exist on the day the clocks go forward is
// passed over that day.

// Named rhythms.
var namedRhythms = map[string]string{
	"hourly":  "0 * * * *",
	"daily":   "0 6 * * *",
	"weekly":  "0 6 * * 1",
	"monthly": "0 6 1 * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// Rhythm is a parsed rhythm.
type Rhythm struct {
	minute, hour, dom, month, dow uint64
	domAny, dowAny                bool
}

// ParseRhythm reads a rhythm.
func ParseRhythm(expr string) (*Rhythm, error) {
	expr = strings.ToLower(strings.TrimSpace(expr))
	if named, ok := namedRhythms[strings.TrimPrefix(expr, "@")]; ok {
		expr = named
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("a rhythm is five fields (minute hour day month weekday) or hourly, daily, weekly, monthly: %q", expr)
	}
	var r Rhythm
	var err error
	if r.minute, err = field(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if r.hour, err = field(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if r.dom, err = field(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if r.month, err = field(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if r.dow, err = field(f[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	if r.dow&(1<<7) != 0 { // 7 is Sunday too
		r.dow |= 1
	}
	r.domAny, r.dowAny = f[2] == "*", f[4] == "*"
	return &r, nil
}

func field(s string, lo, hi int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		rng, step := part, 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return 0, fmt.Errorf("step %q", part[i+1:])
			}
			rng, step = part[:i], n
		}
		from, to := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if from, err = value(a, names); err != nil {
				return 0, err
			}
			if to, err = value(b, names); err != nil {
				return 0, err
			}
		default:
			v, err := value(rng, names)
			if err != nil {
				return 0, err
			}
			from, to = v, v
			if step > 1 {
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return 0, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func value(s string, names map[string]int) (int, error) {
	if v, ok := names[s]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v, nil
}

func (r *Rhythm) dayMatches(t time.Time) bool {
	dom := r.dom&(1<<uint(t.Day())) != 0
	dow := r.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case r.domAny && r.dowAny:
		return true
	case r.domAny:
		return dow
	case r.dowAny:
		return dom
	}
	return dom || dow
}

// Next is the first time after `after` the rhythm names, in loc; the zero
// time when there is none within five years (31 February).
func (r *Rhythm) Next(after time.Time, loc *time.Location) time.Time {
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	end := t.AddDate(5, 0, 0)
	for t.Before(end) {
		if r.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !r.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if r.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if r.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
