//go:build windows

package safefileio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A path past MAX_PATH works as it does in os, even where the system
// long-path setting is off.
func TestCreatePrivateFileLongPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("x", 100), strings.Repeat("y", 100), strings.Repeat("z", 100))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "file")
	require.Greater(t, len(path), 260)

	file, err := CreatePrivateFile(path)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	file, err = OpenCurrentUserFile(path)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}
