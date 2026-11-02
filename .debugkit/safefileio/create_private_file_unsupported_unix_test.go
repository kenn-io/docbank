//go:build unix && !darwin && !linux

package safefileio_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestCreatePrivateFileFailsClosedAndRemovesFileWhenUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")

	file, err := safefileio.CreatePrivateFile(path)
	require.ErrorContains(t, err, "private current-user file validation is unsupported")
	require.Nil(t, file)

	_, err = os.Lstat(path)
	require.ErrorIs(t, err, fs.ErrNotExist)
}
