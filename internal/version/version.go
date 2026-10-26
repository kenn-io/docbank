// Package version carries the build-stamped identity shared by the CLI,
// the daemon runtime record, the OpenAPI document, and self-update.
package version

// Set via -ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
	// Distribution names the package manager that owns this binary, such as
	// "homebrew". Empty means a release archive or source build that may
	// replace itself with docbank update.
	Distribution string
)
