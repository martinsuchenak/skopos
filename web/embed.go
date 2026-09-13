package web

import (
	"embed"
	"fmt"
)

//go:embed all:templates
var TemplateFiles embed.FS

// StaticFiles embeds the built frontend assets.
// Run 'bun install && bun run build' in the web/ directory to generate the dist/ folder.
// Until then, use the placeholder .gitkeep file.
//
//go:embed all:dist
var StaticFiles embed.FS

// VerifyAssets reports whether the embedded dist bundle is a real frontend
// build rather than the .gitkeep placeholder. A binary built without the
// frontend step serves a dead dashboard (every asset 404s) with no error
// anywhere, so serve refuses to start and release tests fail when this
// returns an error.
func VerifyAssets() error {
	const minAppJS = 1024
	data, err := StaticFiles.ReadFile("dist/app.js")
	if err != nil {
		return fmt.Errorf("embedded web assets are missing (dist/app.js): %w — run 'task frontend-build' and rebuild, or deploy a release archive (the release pipeline builds the frontend)", err)
	}
	if len(data) < minAppJS {
		return fmt.Errorf("embedded dist/app.js is only %d bytes — the frontend build looks truncated; re-run 'task frontend-build' and rebuild", len(data))
	}
	return nil
}
