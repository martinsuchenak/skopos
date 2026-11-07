package cmd

import (
	"slices"
	"testing"
)

// Failure messages never show the URL's password; the worker's redactor
// learns the header values, the token after an auth scheme, and the URL's
// credentials.
func TestLogOutputSecrets(t *testing.T) {
	if got := redactedURL("https://user:hunter2hunter2@logs.example/insert/jsonline"); got != "https://user:xxxxx@logs.example/insert/jsonline" {
		t.Fatalf("redacted: %s", got)
	}
	s := logOutputSettings{
		URL:    "https://user:hunter2hunter2@logs.example/insert/jsonline",
		Header: "Authorization: Basic dXNlcjpwYXNzd29yZA==; X-Scope-OrgID: tenant-1",
	}
	got := s.secrets()
	for _, want := range []string{"Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA==", "tenant-1", "hunter2hunter2", "user:hunter2hunter2"} {
		if !slices.Contains(got, want) {
			t.Errorf("secrets %q lack %q", got, want)
		}
	}
	if len((logOutputSettings{URL: "http://localhost:9428/insert/jsonline"}).secrets()) != 0 {
		t.Fatal("no credentials, no secrets")
	}
}
