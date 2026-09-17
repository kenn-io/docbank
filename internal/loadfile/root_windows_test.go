//go:build windows

package loadfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenRejectsWindowsDeviceAndUNCPaths(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "a.pdf"), []byte("%PDF-1.7\n"), 0o600))
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	volume := Volume{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}
	for name, relPath := range map[string]string{
		"unc": `\\server\share\a.pdf`, "device": `\\.\PhysicalDrive0`,
		"long_path_form": `\\?\C:\Windows\win.ini`, "drive_relative": `C:a.pdf`,
		"drive_absolute": `C:\Windows\win.ini`, "reserved_con": `CON`,
		"reserved_aux": `sub\AUX.pdf`, "alt_stream": `a.pdf:secret`,
		"trailing_dot": `a.pdf.`, "backslash_up": `..\..\Windows\win.ini`,
	} {
		_, err = resolver.Open(volume, relPath)
		require.ErrorIs(t, err, ErrUnsafeReference, name)
	}
	resolved, err := resolver.Open(volume, "a.pdf")
	require.NoError(t, err)
	require.NoError(t, resolved.Close())
}
