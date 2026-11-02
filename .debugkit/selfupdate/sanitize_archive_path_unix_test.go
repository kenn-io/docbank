//go:build !windows

package selfupdate

import "testing"

// On Unix a backslash is an ordinary filename byte, so names that would be
// separators, drives, or devices on Windows are single local entries here.
func TestSanitizeArchivePathUnixBackslashNames(t *testing.T) {
	t.Parallel()

	destDir := t.TempDir()
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "backslash traversal is one name", path: `a\..\..\x`, want: `a\..\..\x`},
		{name: "drive letter is one name", path: "C:x", want: "C:x"},
		{name: "device name is ordinary", path: "NUL", want: "NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertSanitizedArchivePath(t, destDir, tt.path, tt.want)
		})
	}
}
