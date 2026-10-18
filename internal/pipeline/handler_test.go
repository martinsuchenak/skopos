package pipeline

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// The REST surface maps the service's outcomes to status codes.
func TestHandlerStatusCodes(t *testing.T) {
	s, _, _ := fixture(t)
	h := NewHandler(s, auth.NewAuthenticator("root-key", nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/workspaces/{id}/pipeline", h.GetWorkspace)
	mux.HandleFunc("PUT /api/workspaces/{id}/pipeline", h.PutWorkspace)
	mux.HandleFunc("PUT /api/groups/{id}/pipeline", h.PutGroup)
	do := func(method, path, body string, key bool) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key {
			req.Header.Set("Authorization", "Bearer root-key")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, _ := do("GET", "/api/workspaces/fx%2Fapi/pipeline", "", false); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	code, body := do("PUT", "/api/workspaces/fx%2Fapi/pipeline", `{"stages":{"review":{"members":[{"runner":"claude","focus":"security"}]}},"via":"dashboard"}`, true)
	if code != http.StatusOK || !strings.Contains(body, `"source":"workspace"`) {
		t.Fatalf("put: %d %s", code, body)
	}
	if code, body := do("PUT", "/api/workspaces/fx%2Fapi/pipeline", `{"stages":{"implement":{"members":[{"runner":"a"},{"runner":"b"}]}}}`, true); code != http.StatusBadRequest {
		t.Fatalf("invalid settings: %d %s", code, body)
	}
	if code, _ := do("GET", "/api/workspaces/nope/pipeline", "", true); code != http.StatusNotFound {
		t.Fatalf("unknown workspace: %d", code)
	}
	if code, _ := do("PUT", "/api/groups/fortix/pipeline", `{"stages":{"qa":{"members":[{"runner":"claude"}]}}}`, true); code != http.StatusOK {
		t.Fatalf("group put: %d", code)
	}
	if code, body := do("PUT", "/api/groups/web-team/pipeline", `{"stages":{"qa":{"members":[{"runner":"glm"}]}}}`, true); code != http.StatusConflict {
		t.Fatalf("group conflict: %d %s", code, body)
	}
}
