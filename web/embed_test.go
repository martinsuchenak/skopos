package web

import "testing"

// The release pipeline builds the frontend before running tests, so a missing
// or truncated bundle must fail there. Local checkouts without a build skip —
// compiling still works via the committed dist/.gitkeep placeholder.
func TestVerifyAssets(t *testing.T) {
	if _, err := StaticFiles.ReadFile("dist/app.js"); err != nil {
		t.Skip("frontend not built locally (run task frontend-build); release builds gate this")
	}
	if err := VerifyAssets(); err != nil {
		t.Fatal(err)
	}
}
