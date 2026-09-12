//go:build windows

package emailmime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/docbank/internal/winsecurity"
)

func TestWindowsSpoolAcceptsNativePathAliases(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "Synthetic Spool Parent")
	require.NoError(t, os.Mkdir(parent, 0o700))
	longPath, err := windows.UTF16PtrFromString(parent)
	require.NoError(t, err)
	size, err := windows.GetShortPathName(longPath, nil, 0)
	require.NoError(t, err)
	shortPath := make([]uint16, size)
	_, err = windows.GetShortPathName(longPath, &shortPath[0], size)
	require.NoError(t, err)
	for name, path := range map[string]string{
		"temporary": parent,
		"case":      strings.ToLower(parent),
		"short":     windows.UTF16ToString(shortPath),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "short" && path == parent {
				t.Skip("temporary filesystem does not supply short path aliases")
			}
			spool, err := createSpool(path)
			require.NoError(t, err)
			require.NoError(t, spool.cleanup())
			_, err = RecoverStale(t.Context(), path)
			require.NoError(t, err)
		})
	}
}

func TestWindowsSpoolIsPrivateAndRejectsReparseArtifact(t *testing.T) {
	result := decodeFixture(t, "\r\nbody")
	var filename string
	for _, item := range result.artifacts {
		if item.artifact.PartPath == "1" && item.artifact.Reference.Role == "decoded_payload" {
			filename = item.filename
			break
		}
	}
	require.NotEmpty(t, filename)
	path := filepath.Join(result.spool.parent.Name(), result.spool.name, filename)
	file, err := winsecurity.OpenRestrictedCurrentUserFile(path)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, result.spool.root.Rename(filename, filename+"-original"))
	require.NoError(t, os.Symlink(filename+"-original", path))
	_, err = result.OpenArtifact(t.Context(), "1", "decoded_payload")
	require.Error(t, err)
}
