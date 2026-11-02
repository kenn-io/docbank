// Package shellquote quotes arguments embedded in Git shell command strings.
package shellquote

import "strings"

// Single returns value enclosed in POSIX shell single quotes.
func Single(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
