// Package usage is what a run's agents used, as their runners reported it:
// counts by name, and the same per model where a runner names the model.
// The worker builds a record per run (internal/worker/usage.go reads each
// runner's output into it), the server stores and sums it (internal/runs),
// the item report and the dashboard show it. Names runners share mean the
// same thing whichever runner reported them:
//
//	input_tokens        input not read from or written to a cache
//	cache_read_tokens   input read from the provider's prompt cache
//	cache_write_tokens  input written to it
//	output_tokens       output, reasoning included where the runner counts it so
//	reasoning_tokens    the reasoning (thinking) part
//	turns               model turns
//	agent_calls, agent_ms  times the worker started the agent, and for how long
//
// Any other count keeps the runner's name, nested names joined by dots.
// Recorded, never decided on, and never priced.
package usage

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Usage is one record: a run's, or a sum of runs'.
type Usage struct {
	Counts map[string]int64            `json:"counts,omitempty"`
	Models map[string]map[string]int64 `json:"models,omitempty"`
}

// Bounds of a stored record (Add, Count, ModelCount keep to them; Sum does
// not, since it only reads).
const (
	MaxCounts      = 48
	MaxModels      = 8
	MaxModelCounts = 16
	MaxModelName   = 128
)

var keyRule = regexp.MustCompile(`^[a-z0-9][a-z0-9_.]{0,63}$`)

// ValidKey: a count's name may be stored.
func ValidKey(k string) bool { return keyRule.MatchString(k) }

func validModel(m string) bool {
	if m == "" || len(m) > MaxModelName || strings.TrimSpace(m) != m {
		return false
	}
	return strings.IndexFunc(m, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

// Count adds v to the count k, within the bounds: a name past them is left
// out, a count that is not positive is ignored.
func (u *Usage) Count(k string, v int64) {
	if v <= 0 || !ValidKey(k) {
		return
	}
	if u.Counts == nil {
		u.Counts = map[string]int64{}
	}
	if _, ok := u.Counts[k]; ok || len(u.Counts) < MaxCounts {
		u.Counts[k] += v
	}
}

// ModelCount adds v to model's count k, within the bounds.
func (u *Usage) ModelCount(model, k string, v int64) {
	model = strings.TrimSpace(model)
	if v <= 0 || !validModel(model) || !ValidKey(k) {
		return
	}
	if u.Models == nil {
		u.Models = map[string]map[string]int64{}
	}
	c, ok := u.Models[model]
	if !ok {
		if len(u.Models) >= MaxModels {
			return
		}
		c = map[string]int64{}
		u.Models[model] = c
	}
	if _, ok := c[k]; ok || len(c) < MaxModelCounts {
		c[k] += v
	}
}

// Add sums o into u, within the bounds.
func (u *Usage) Add(o *Usage) {
	if o == nil {
		return
	}
	for k, v := range o.Counts {
		u.Count(k, v)
	}
	for m, c := range o.Models {
		for k, v := range c {
			u.ModelCount(m, k, v)
		}
	}
}

// Sum adds records up for reading (an item's total), without the bounds: a
// name one record alone uses is never dropped from the sum. nil when there
// is nothing.
func Sum(us ...*Usage) *Usage {
	sum := &Usage{}
	for _, u := range us {
		if u == nil {
			continue
		}
		for k, v := range u.Counts {
			if sum.Counts == nil {
				sum.Counts = map[string]int64{}
			}
			sum.Counts[k] += v
		}
		for m, c := range u.Models {
			if sum.Models == nil {
				sum.Models = map[string]map[string]int64{}
			}
			if sum.Models[m] == nil {
				sum.Models[m] = map[string]int64{}
			}
			for k, v := range c {
				sum.Models[m][k] += v
			}
		}
	}
	if sum.Empty() {
		return nil
	}
	return sum
}

// Empty: nothing recorded.
func (u *Usage) Empty() bool { return u == nil || (len(u.Counts) == 0 && len(u.Models) == 0) }

// TokenKeys: the token counts, the ones a model's line keeps.
var TokenKeys = []string{"input_tokens", "cache_read_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens"}

// ForModel files the record's token counts under the one model that did
// the work, for a runner that does not count per model (one that does
// keeps its own breakdown).
func (u *Usage) ForModel(model string) {
	if u == nil || len(u.Models) > 0 {
		return
	}
	for _, k := range TokenKeys {
		u.ModelCount(model, k, u.Counts[k])
	}
}

// ModelNames: the models the record names, sorted.
func (u *Usage) ModelNames() []string {
	if u == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(u.Models))
}

// Tokens is the input (the cache's included) and the output.
func (u *Usage) Tokens() (in, out int64) {
	if u == nil {
		return 0, 0
	}
	return u.Counts["input_tokens"] + u.Counts["cache_read_tokens"] + u.Counts["cache_write_tokens"], u.Counts["output_tokens"]
}

// Compact: 980, 1.2k, 56k, 4.1M (the dashboard's compactCount rounds the
// same way).
func Compact(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
