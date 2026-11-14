// Package labels holds the one rule for worker labels and the labels an
// item needs: what a label looks like, how a list is normalised, and when a
// worker's labels cover what an item needs.
package labels

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Max is how many labels a worker may hold, or an item may need.
const Max = 8

// ErrInvalid marks a list that Normalize refuses.
var ErrInvalid = errors.New("invalid labels")

var reLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// Normalize returns the list lower-cased, without blanks and duplicates,
// sorted. A label is 1 to 32 characters: letters, digits, '_', '.', '-',
// starting with a letter or digit. An empty list is nil.
func Normalize(in []string) ([]string, error) {
	var out []string
	for _, l := range in {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" {
			continue
		}
		if !reLabel.MatchString(l) {
			return nil, fmt.Errorf("%w: %q is not a label (1 to 32 of a-z, 0-9, '_', '.', '-', starting with a letter or digit)", ErrInvalid, l)
		}
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	if len(out) > Max {
		return nil, fmt.Errorf("%w: at most %d labels", ErrInvalid, Max)
	}
	slices.Sort(out)
	return out, nil
}

// Split reads a comma-separated list ("stock, plant-2") for Normalize.
func Split(s string) []string {
	return strings.Split(s, ",")
}

// Arg reads a command-line or tool argument that sets a list: "" leaves it
// unchanged (set = false), "-" clears it, anything else is a
// comma-separated list for Normalize.
func Arg(s string) (list []string, set bool) {
	switch s = strings.TrimSpace(s); s {
	case "":
		return nil, false
	case "-":
		return []string{}, true
	}
	return Split(s), true
}

// Covers reports whether a worker with the labels `has` may take work that
// needs `needs`: every needed label is among them. Nothing needed is
// covered by any worker.
func Covers(has, needs []string) bool {
	for _, n := range needs {
		if !slices.Contains(has, n) {
			return false
		}
	}
	return true
}

// Missing lists the needed labels that are not in `has`.
func Missing(has, needs []string) []string {
	var out []string
	for _, n := range needs {
		if !slices.Contains(has, n) {
			out = append(out, n)
		}
	}
	return out
}

// JSON is the stored form: a JSON array, "" for no labels.
func JSON(list []string) string {
	if len(list) == 0 {
		return ""
	}
	b, err := json.Marshal(list)
	if err != nil {
		return ""
	}
	return string(b)
}

// Parse reads the stored form; anything unreadable is no labels.
func Parse(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out) == 0 {
		return nil
	}
	return out
}
