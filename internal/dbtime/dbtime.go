// Package dbtime is the one timestamp format skopos writes to SQLite.
//
// Timestamps are stored as text and compared and ordered as text in SQL
// (queue FIFO, lease expiry, timelines). time.RFC3339Nano trims trailing
// zeros, so its strings do not sort in time order within a second
// ("…05.12345Z" sorts after "…05.123456Z", "…05Z" after "…05.5Z"), and a
// non-UTC time carries an offset that breaks ordering entirely. This
// format is always UTC with exactly nine fractional digits: fixed width, so
// text order is time order.
//
// Parse accepts this format and every older RFC3339 / RFC3339Nano string,
// so rows written before the switch still read back; db.RunMigrations
// rewrites them to this format once.
package dbtime

import "time"

// Layout is fixed-width UTC with nanoseconds. The trailing Z is literal:
// Format always converts to UTC first.
const Layout = "2006-01-02T15:04:05.000000000Z"

// Width is the length of every formatted timestamp.
const Width = len(Layout)

// Format renders t for storage.
func Format(t time.Time) string { return t.UTC().Format(Layout) }

// Parse reads a stored timestamp (this format or legacy RFC3339[Nano]).
func Parse(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
