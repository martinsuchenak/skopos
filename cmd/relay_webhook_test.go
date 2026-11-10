package cmd

import "testing"

func TestEndpointOnly(t *testing.T) {
	for in, want := range map[string]string{
		"https://jobs.example/hooks?token=abc#x": "https://jobs.example/hooks",
		"https://jobs.example:8443/a/b":          "https://jobs.example:8443/a/b",
		"://bad":                                 "the endpoint",
	} {
		if got := endpointOnly(in); got != want {
			t.Errorf("endpointOnly(%q) = %q, want %q", in, got, want)
		}
	}
}
