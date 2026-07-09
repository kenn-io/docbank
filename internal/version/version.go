// Package version carries the build-stamped identity shared by the CLI,
// the daemon runtime record, the OpenAPI document, and self-update.
package version

// Set via -ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
)
