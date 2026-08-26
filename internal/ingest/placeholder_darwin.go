//go:build darwin

package ingest

import (
	"errors"
	"io/fs"
	"syscall"
)

// sfDataless is the BSD file flag macOS sets on a cloud-provider placeholder
// whose bytes are not present locally (File Provider / iCloud Drive / Google
// Drive for Desktop). The syscall package does not export it.
const sfDataless = 0x40000000

// isCloudPlaceholder reports whether info describes a dataless file: metadata
// is present, but opening it makes the cloud provider fetch the content, or
// refuse to for the calling process.
func isCloudPlaceholder(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}

// placeholderReadHint explains a read failure that macOS reports when a cloud
// provider declines to hydrate a placeholder for the reading process. A daemon
// launched by launchd as a background job typically sees this; the same file
// opens fine from an interactive session, after which the content is cached
// and a retried ingest succeeds.
func placeholderReadHint(err error) string {
	if errors.Is(err, syscall.EDEADLK) {
		return "the cloud provider did not hydrate this placeholder for the daemon; " +
			"open the file once from a user session (or make it available offline), then retry"
	}
	return ""
}
