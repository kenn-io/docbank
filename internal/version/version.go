// Package version carries the build-stamped identity shared by the CLI,
// the daemon runtime record, the OpenAPI document, and self-update.
package version

// Set via -ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
	// Distribution names who shipped this binary: "kenn" for official
	// direct downloads, or a package manager such as "homebrew" that owns
	// upgrades. Empty means a local or source build. Only a package manager
	// stops docbank update from replacing the binary.
	Distribution string
)
