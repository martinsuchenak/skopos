package web

import "embed"

//go:embed all:templates
var TemplateFiles embed.FS

// StaticFiles embeds the built frontend assets.
// Run 'bun install && bun run build' in the web/ directory to generate the dist/ folder.
// Until then, use the placeholder .gitkeep file.
//
//go:embed all:dist
var StaticFiles embed.FS
