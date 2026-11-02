//go:build darwin || linux || windows

package atomicfile_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/atomicfile"
)

func TestRenameNoReplace(t *testing.T) {
	t.Run("renames to a free name", func(t *testing.T) {
		dir := t.TempDir()
		src := writeString(t, dir, "src", "data")
		dst := filepath.Join(dir, "dst")

		require.NoError(t, atomicfile.RenameNoReplace(src, dst))

		assert.Equal(t, "data", readString(t, dst))
		assert.Equal(t, []string{"dst"}, entryNames(t, dir))
	})
	t.Run("refuses an existing destination", func(t *testing.T) {
		dir := t.TempDir()
		src := writeString(t, dir, "src", "new")
		dst := writeString(t, dir, "dst", "old")

		err := atomicfile.RenameNoReplace(src, dst)

		require.ErrorIs(t, err, fs.ErrExist)
		var linkErr *os.LinkError
		require.ErrorAs(t, err, &linkErr)
		assert.Equal(t, "new", readString(t, src))
		assert.Equal(t, "old", readString(t, dst))
	})
}
