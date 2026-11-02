//go:build unix && !darwin && !linux

package atomicfile_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/atomicfile"
)

// Without an atomic no-replace rename, RenameNoReplace fails rather than
// checking and renaming separately.
func TestRenameNoReplaceUnsupported(t *testing.T) {
	dir := t.TempDir()
	src := writeString(t, dir, "src", "data")

	err := atomicfile.RenameNoReplace(src, filepath.Join(dir, "dst"))

	require.ErrorIs(t, err, errors.ErrUnsupported)
	assert.Equal(t, "data", readString(t, src))
	assert.Equal(t, []string{"src"}, entryNames(t, dir))
}
