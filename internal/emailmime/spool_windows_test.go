//go:build windows

package emailmime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/winsecurity"
)

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
