package wsconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Checking a config before it is saved, field by field: a form shows each
// problem next to the field it is about, and all of them at once, where a
// save reports only the first.

// Problem is one thing wrong with a config. Field is named as Fields names
// it ("checks", "push.kind", "stages.qa"); "" = the config as a whole.
type Problem struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// CheckResult is what a check says: the problems (none = the save would be
// accepted) and the fields the save would change.
type CheckResult struct {
	Problems []Problem `json:"problems"`
	Changed  []string  `json:"changed"`
}

// nested are the fields whose keys are checked one by one.
var nested = map[string]bool{"push": true, "stages": true}

// plain strips the sentinel's text from a message shown next to a field.
func plain(err error) string {
	return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
}

// decodeField reads a one-field document strictly, with what the JSON
// decoder says turned into words about the value.
func decodeField(doc []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	err := dec.Decode(&c)
	if err == nil {
		return c, nil
	}
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &typeErr):
		want := map[string]string{"int": "a whole number", "bool": "true or false", "string": "text", "slice": "a list", "struct": "an object", "map": "an object", "ptr": "an object"}[typeErr.Type.Kind().String()]
		if want == "" {
			want = "another kind of value"
		}
		return c, fmt.Errorf("takes %s, not %s", want, typeErr.Value)
	case strings.Contains(err.Error(), "unknown field"):
		return c, fmt.Errorf("is not a field of the config")
	case strings.Contains(err.Error(), "invalid duration"), strings.Contains(err.Error(), "missing unit in duration"), strings.Contains(err.Error(), "unknown unit"):
		return c, fmt.Errorf("is not a duration: write it like 90s, 10m or 1h30m")
	}
	return c, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "json: "))
}

// FieldProblems checks a config document one field at a time and returns
// every problem, by field. A document that is not an object is one problem
// of the whole.
func FieldProblems(raw []byte) []Problem {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return []Problem{{Message: "the config is a JSON object of fields"}}
	}
	var out []Problem
	check := func(field string, doc []byte) {
		c, err := decodeField(doc)
		if err == nil {
			_, err = Normalize(c)
		}
		if err != nil {
			msg := plain(err)
			// A message that already starts with the field's name reads
			// twice next to the field.
			msg = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(msg, field), ":"))
			out = append(out, Problem{Field: field, Message: msg})
		}
	}
	wrap := func(key string, val json.RawMessage) []byte {
		b, _ := json.Marshal(map[string]json.RawMessage{key: val})
		return b
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		val := top[key]
		if !nested[key] {
			check(key, wrap(key, val))
			continue
		}
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(val, &inner); err != nil {
			out = append(out, Problem{Field: key, Message: "takes an object"})
			continue
		}
		subs := make([]string, 0, len(inner))
		for k := range inner {
			subs = append(subs, k)
		}
		sort.Strings(subs)
		for _, sub := range subs {
			check(key+"."+sub, wrap(key, wrap(sub, inner[sub])))
		}
	}
	return out
}

// problemField names the field a whole-config error is about, when its
// message starts with one ("push.remote_url is needed …"); "" otherwise.
func problemField(c Config, msg string) string {
	best := ""
	for name := range c.Fields() {
		if strings.HasPrefix(msg, name) && len(name) > len(best) {
			best = name
		}
	}
	for _, name := range []string{"push.remote_url", "push.kind", "push.pull_request", "mcp_servers", "mcp", "stages"} {
		if strings.HasPrefix(msg, name) && len(name) > len(best) {
			best = name
		}
	}
	return best
}

// Check says what a save of this document would do, writing nothing: every
// field's own problems, then — when each field is sound — what only the
// whole config or its place among the groups shows (a push without a
// remote, an MCP server the workspace may not use, a field another group
// sets). It needs what the save needs: the admin permission at the owner's
// level.
func (s *Service) Check(ctx context.Context, kind, id string, raw []byte) (*CheckResult, error) {
	res := &CheckResult{Problems: FieldProblems(raw), Changed: []string{}}
	if res.Problems == nil {
		res.Problems = []Problem{} // a list in the answer, also when empty
	}
	if len(res.Problems) > 0 {
		// Still refuse a caller who may not save here.
		if _, err := s.Plan(ctx, kind, id, &Config{}); err != nil && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) {
			return nil, err
		}
		return res, nil
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		res.Problems = []Problem{{Message: strings.TrimPrefix(err.Error(), "json: ")}}
		return res, nil
	}
	changed, err := s.Plan(ctx, kind, id, &c)
	switch {
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrConflict):
		msg := strings.TrimPrefix(plain(err), ErrConflict.Error()+": ")
		field := problemField(c, msg)
		// "mcp: server … is not defined" reads twice next to the field.
		res.Problems = []Problem{{Field: field, Message: strings.TrimPrefix(msg, field+": ")}}
	case err != nil:
		return nil, err
	default:
		if changed != nil {
			res.Changed = changed
		}
	}
	return res, nil
}
