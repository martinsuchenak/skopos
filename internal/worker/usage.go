package worker

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode"

	"github.com/martinsuchenak/skopos/internal/usage"
)

// Usage is what an agent used, as its runner reported it: the shared record
// (internal/usage), summed over a run's agent calls and sent with its
// report. Every runner reads its own output into it here: usageFrom takes
// every count of a runner's usage object, under usageNames' shared names
// where a runner names one of them its own way.
type Usage = usage.Usage

// usageNames: the names runners give the shared counts.
var usageNames = map[string]string{
	"cached_input_tokens":                    "cache_read_tokens",
	"cache_read_input_tokens":                "cache_read_tokens",
	"cache_read_tokens":                      "cache_read_tokens",
	"cache.read":                             "cache_read_tokens",
	"cache_creation_input_tokens":            "cache_write_tokens",
	"cache_write_input_tokens":               "cache_write_tokens",
	"cache.write":                            "cache_write_tokens",
	"input":                                  "input_tokens",
	"output":                                 "output_tokens",
	"reasoning":                              "reasoning_tokens",
	"reasoning_output_tokens":                "reasoning_tokens",
	"thinking_tokens":                        "reasoning_tokens",
	"output_tokens_details.thinking_tokens":  "reasoning_tokens",
	"output_tokens_details.reasoning_tokens": "reasoning_tokens",
	"server_tool_use.web_search_requests":    "web_search_requests",
	"server_tool_use.web_fetch_requests":     "web_fetch_requests",
	"total":                                  "total_tokens",
}

// usageFrom reads a runner's usage object: every number in it, nested
// objects flattened with dots, arrays and anything else left out, named by
// usageNames. skip names leaves that are limits, not counts.
func usageFrom(raw json.RawMessage, skip ...string) map[string]int64 {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	out := map[string]int64{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, sub := range t {
				name := snake(k)
				if prefix != "" {
					name = prefix + "." + name
				}
				walk(name, sub)
			}
		case float64:
			if t < 0 || t != float64(int64(t)) || slices.Contains(skip, prefix) {
				return // a ratio, a price or a limit: not a count
			}
			name := prefix
			if n, ok := usageNames[name]; ok {
				name = n
			}
			if usage.ValidKey(name) {
				out[name] += int64(t)
			}
		}
	}
	walk("", v)
	if len(out) == 0 {
		return nil
	}
	return out
}

// snake: claude's per-model names are camelCase (cacheReadInputTokens,
// costUSD → cost_usd).
func snake(s string) string {
	var b strings.Builder
	prevUpper := true
	for _, r := range s {
		up := unicode.IsUpper(r)
		if up && !prevUpper {
			b.WriteByte('_')
		}
		prevUpper = up
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// usageBrief: "tokens: 12k in, 4.1M from cache, 980 out (model)" for the
// run log, "" when the call reported no tokens.
func usageBrief(u *Usage) string {
	if u.Empty() {
		return ""
	}
	var parts []string
	for _, p := range []struct{ k, label string }{
		{"input_tokens", "in"}, {"cache_read_tokens", "from cache"}, {"cache_write_tokens", "to cache"},
		{"output_tokens", "out"}, {"reasoning_tokens", "reasoning"},
	} {
		if v := u.Counts[p.k]; v > 0 {
			parts = append(parts, usage.Compact(v)+" "+p.label)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	line := "tokens: " + strings.Join(parts, ", ")
	if ms := u.ModelNames(); len(ms) > 0 {
		line += " (" + strings.Join(ms, ", ") + ")"
	}
	return line
}
