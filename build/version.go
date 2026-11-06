// Package build holds version information injected at build time with
// -ldflags: by Taskfile.yml, the Dockerfile and GoReleaser (.goreleaser.yml).
package build

var (
	// Version is the release version (0.20.0 from GoReleaser), or git
	// describe output for a local build. The worker's self-upgrade compares
	// it with the server's.
	Version = "dev"
	// Commit is the short commit hash the binary was built from.
	Commit = "unknown"
	// Date is when the binary was built (RFC 3339, UTC).
	Date = "unknown"
)

// String describes the build for --version: "0.20.0 (abc1234, 2026-11-06T10:00:00Z)".
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
