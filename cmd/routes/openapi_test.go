package routes

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestOpenAPIMatchesRoutes keeps openapi.yaml in lockstep with the HTTP API:
// every API route registered in code must appear in the spec with the right
// method, and the spec must not advertise paths that don't exist. Non-REST
// surfaces (dashboard, static files, the MCP protocol endpoint at /mcp) are
// excluded on both sides.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	routes := registeredRoutes(t)
	spec := specOperations(t)

	for _, r := range routes {
		methods, ok := spec[r.path]
		if !ok {
			t.Errorf("openapi.yaml: route %s %s is not in the spec", r.method, r.path)
			continue
		}
		if !methods[r.method] {
			t.Errorf("openapi.yaml: %s is missing operation %s (has %v)", r.path, r.method, methods)
		}
	}
	for path, methods := range spec {
		for method := range methods {
			if !routes.has(method, path) {
				t.Errorf("openapi.yaml: %s %s has no matching route in code", method, path)
			}
		}
	}
}

type route struct{ method, path string }

type routeSet []route

func (rs routeSet) has(method, path string) bool {
	for _, r := range rs {
		if r.method == method && r.path == path {
			return true
		}
	}
	return false
}

var routePattern = regexp.MustCompile(`(?:HandleFunc|Handle)\("(GET|POST|PUT|PATCH|DELETE) ([^"]+)"`)

// nonREST lists registered endpoints that are not part of the REST surface and
// therefore not spec'd as OpenAPI operations.
var nonREST = map[string]bool{
	"GET /":        true, // dashboard
	"GET /static/": true, // embedded frontend assets
	"GET /mcp":     true, // MCP protocol endpoint (mentioned in info.description)
	"POST /mcp":    true,
	"DELETE /mcp":  true,
}

// registeredRoutes extracts "METHOD /path" registrations from the route files
// and serve.go (which mounts events/metrics/mcp itself).
func registeredRoutes(t *testing.T) routeSet {
	t.Helper()
	files := []string{"../serve.go"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			files = append(files, e.Name())
		}
	}
	var out routeSet
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		for _, m := range routePattern.FindAllStringSubmatch(string(raw), -1) {
			if nonREST[m[1]+" "+m[2]] {
				continue
			}
			out = append(out, route{m[1], m[2]})
		}
	}
	if len(out) == 0 {
		t.Fatal("no routes extracted — extraction pattern broken?")
	}
	return out
}

// specOperations parses paths and their operations out of openapi.yaml. The
// spec is written in a consistent style: top-level `paths:` section, paths at
// two-space indent, operation keys at four-space indent.
func specOperations(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatalf("reading openapi.yaml: %v", err)
	}
	spec := map[string]map[string]bool{}
	inPaths := false
	var path string
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && line != "" && !strings.HasPrefix(line, " "):
			inPaths = false // next top-level section (components:, ...)
		case inPaths && strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			path = strings.TrimSpace(line[:len(line)-1])
			spec[path] = map[string]bool{}
		case inPaths && path != "":
			if !strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "     ") {
				continue
			}
			f := strings.Fields(line)
			if len(f) >= 1 && strings.HasSuffix(f[0], ":") {
				method := strings.ToUpper(strings.TrimSuffix(f[0], ":"))
				switch method {
				case "GET", "POST", "PUT", "PATCH", "DELETE":
					spec[path][method] = true
				}
			}
		}
	}
	if len(spec) == 0 {
		t.Fatal("no paths parsed from openapi.yaml — parser broken?")
	}
	return spec
}
