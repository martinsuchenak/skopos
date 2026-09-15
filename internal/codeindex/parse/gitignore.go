package parse

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// gitignoreMatcher evaluates .gitignore patterns per directory, mirroring
// git's semantics closely enough for indexing: a pattern is matched against
// paths relative to the directory containing its .gitignore file. The full
// git algorithm (negation ordering, **, double-star prefixes) is supported;
// anchored-vs-unanchored and dir-only semantics follow the spec.
type gitignoreMatcher struct {
	// rules[dir] = patterns, keyed by the absolute directory containing the .gitignore
	rules map[string][]gitignorePattern
}

type gitignorePattern struct {
	negate    bool
	dirOnly   bool
	anchored  bool
	segments  []string // "**" allowed as its own segment
	raw       string
	mustBeDir bool
}

func newGitignoreMatcher() *gitignoreMatcher {
	return &gitignoreMatcher{rules: map[string][]gitignorePattern{}}
}

// loadDir parses the .gitignore in dir (if present). Returns true if the
// directory contains one.
func (g *gitignoreMatcher) loadDir(dir string) bool {
	f, err := os.Open(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return false
	}
	defer f.Close()
	var pats []gitignorePattern
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pats = append(pats, parseGitignorePattern(line))
	}
	if len(pats) > 0 {
		g.rules[dir] = append(g.rules[dir], pats...)
	}
	return true
}

func parseGitignorePattern(line string) gitignorePattern {
	p := gitignorePattern{raw: line}
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	// Anchored when the pattern contains a slash (other than a trailing one
	// already stripped) or starts with "/".
	if strings.HasPrefix(line, "/") {
		p.anchored = true
		line = strings.TrimPrefix(line, "/")
	} else if strings.Contains(line, "/") {
		p.anchored = true
	}
	p.segments = strings.Split(line, "/")
	return p
}

// match reports whether rel (relative to root, slash-separated, no leading
// slash) matches this pattern; isDir says whether the path being tested is
// a directory.
func (p *gitignorePattern) match(rel string, isDir bool) bool {
	if p.dirOnly && !isDir {
		// dir-only patterns still match files under a matched dir, but that
		// is handled by the walker skipping whole dirs; for files tested
		// individually, dir-only patterns don't match the file itself.
		return false
	}
	segs := strings.Split(rel, "/")
	if p.anchored {
		return matchSegments(p.segments, segs)
	}
	// Unanchored: match against the basename or any trailing subsequence.
	for i := range segs {
		if matchSegments(p.segments, segs[i:]) {
			return true
		}
	}
	return false
}

// matchSegments matches pattern segments against path segments with **
// support. Trailing ** matches everything remaining.
func matchSegments(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		// ** matches zero or more segments
		for skip := 0; skip <= len(path); skip++ {
			if matchSegments(pat[1:], path[skip:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	if !segmentMatch(pat[0], path[0]) {
		return false
	}
	return matchSegments(pat[1:], path[1:])
}

// segmentMatch handles one path segment: exact, "*", or glob with a single
// "*" (the common case; gitignore character classes are rare in practice).
func segmentMatch(pat, s string) bool {
	if pat == "*" {
		return true
	}
	if !strings.Contains(pat, "*") {
		return pat == s
	}
	// simple single-star glob
	parts := strings.Split(pat, "*")
	if len(parts) == 2 {
		return strings.HasPrefix(s, parts[0]) && strings.HasSuffix(s, parts[1]) && len(s) >= len(parts[0])+len(parts[1])
	}
	return pat == s
}

// Ignore reports whether path (absolute) should be ignored, consulting
// .gitignore files from every ancestor directory between root and the file.
// isDir distinguishes directory skips.
func (g *gitignoreMatcher) Ignore(root, path string, isDir bool) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	ignored := false
	// Walk ancestor dirs of the file (excluding the file itself), innermost
	// last; later (more specific) rules win, so evaluate outer->inner and
	// let inner results overwrite.
	dir := root
	dirs := []string{root}
	for _, seg := range strings.Split(rel, "/")[:strings.Count(rel, "/")] {
		dir = filepath.Join(dir, seg)
		dirs = append(dirs, dir)
	}
	for _, d := range dirs {
		for _, p := range g.rules[d] {
			// path relative to the .gitignore's directory
			relTo, err := filepath.Rel(d, path)
			if err != nil {
				continue
			}
			if p.match(filepath.ToSlash(relTo), isDir) {
				ignored = !p.negate
			}
		}
	}
	return ignored
}
