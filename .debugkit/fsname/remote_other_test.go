//go:build !linux && !darwin && !dragonfly && !freebsd && !openbsd && !windows

package fsname

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteUnsupported(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()

	remote, err := Remote(dir)
	require.ErrorIs(err, errors.ErrUnsupported)
	assert.False(t, remote)

	f, err := os.Open(dir)
	require.NoError(err)
	t.Cleanup(func() { _ = f.Close() })
	remote, err = RemoteFile(f)
	require.ErrorIs(err, errors.ErrUnsupported)
	assert.False(t, remote)
}
