//go:build unix

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteNewSyncsDirectoryReachedThroughSymlinkThenDotDot(t *testing.T) {
	require := require.New(t)
	root := realTempDir(t)
	require.NoError(os.MkdirAll(filepath.Join(root, "x", "sub"), 0o700))
	require.NoError(os.Mkdir(filepath.Join(root, "a"), 0o700))
	require.NoError(os.Symlink(filepath.Join(root, "x", "sub"), filepath.Join(root, "a", "hop")))
	var synced []string
	injectSyncDir(t, func(d string) error {
		synced = append(synced, d)
		return nil
	})

	require.NoError(WriteNew(filepath.Join(root, "a", "hop")+"/../target", []byte("new")))

	assert.Equal(t, []string{filepath.Join(root, "x")}, synced)
	assert.Equal(t, "new", readFile(t, filepath.Join(root, "x", "target")))
}
