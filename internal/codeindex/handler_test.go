package codeindex

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// newManifestTestHandler builds a handler over a temp index store with
// loopback auth disabled (authenticator accepts everything).
func newManifestTestHandler(t *testing.T) *Handler {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return NewHandler(NewService(store), auth.NewAuthenticator("", nil))
}

// TestManifestBodyLimit verifies large manifests (one entry per file; big
// repos exceed the 1 MiB default JSON cap) decode under the commit-sized
// cap, and that a genuinely oversized body surfaces 413 rather than a
// misleading 400 "invalid request body".
func TestManifestBodyLimit(t *testing.T) {
	h := newManifestTestHandler(t)

	var buf bytes.Buffer
	buf.WriteString(`{"files":[`)
	entry := []byte(`{"path":"pkg/sub/deep/file_with_a_reasonably_long_name_0001.go","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},`)
	for buf.Len() < 2<<20 {
		buf.Write(entry)
	}
	buf.WriteString(`{"path":"z","hash":"bbbb"}]}`) // ~2 MiB: over 1 MiB, under 64 MiB

	req := httptest.NewRequest("POST", "/api/codeindex/ws-x/manifest", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("workspace", "ws-x")
	w := httptest.NewRecorder()
	h.Manifest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("2 MiB manifest must decode under the commit-sized cap, got %d %s", w.Code, w.Body.String())
	}

	// Valid JSON whose string value alone exceeds the cap: the decoder
	// must consume past the limit to parse it, tripping MaxBytesReader —
	// a malformed payload would fail on syntax before the size check.
	var big bytes.Buffer
	big.WriteString(`{"files":[{"path":"`)
	big.Write(bytes.Repeat([]byte("a"), (64<<20)+4096))
	big.WriteString(`","hash":"x"}]}`)
	tooBig := big.Bytes()
	req2 := httptest.NewRequest("POST", "/api/codeindex/ws-x/manifest", bytes.NewReader(tooBig))
	req2.Header.Set("Content-Type", "application/json")
	req2.SetPathValue("workspace", "ws-x")
	w2 := httptest.NewRecorder()
	h.Manifest(w2, req2)
	if w2.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body must be 413, got %d", w2.Code)
	}
}
