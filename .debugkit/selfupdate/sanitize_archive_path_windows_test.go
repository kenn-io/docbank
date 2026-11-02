package selfupdate

import "testing"

func TestSanitizeArchivePathWindows(t *testing.T) {
	t.Parallel()

	destDir := t.TempDir()
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "backslash nested", path: `bin\tool`, want: "bin/tool"},
		{name: "backslash leading dots in name", path: `a\..b`, want: "a/..b"},
		{name: "drive relative", path: "C:x"},
		{name: "drive absolute backslash", path: `C:\Windows\x`},
		{name: "drive absolute slash", path: "C:/Windows/x"},
		{name: "unc share", path: `\\server\share`},
		{name: "rooted backslash", path: `\Windows\x`},
		{name: "reserved device", path: "NUL"},
		{name: "nested lowercase reserved device", path: "a/con"},
		{name: "backslash traversal", path: `a\..\..\x`},
		{name: "mixed separator traversal", path: `a/..\..\x`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertSanitizedArchivePath(t, destDir, tt.path, tt.want)
		})
	}
}
