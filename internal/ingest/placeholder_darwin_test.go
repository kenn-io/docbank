//go:build darwin

package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An ordinary local file is never a placeholder: SF_DATALESS is set by the
// provider, and no test may depend on a real cloud mount.
func TestIsCloudPlaceholderFalseForLocalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.bin")
	require.NoError(t, os.WriteFile(path, []byte("bytes"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, isCloudPlaceholder(info))
}

func TestPlaceholderReadHint(t *testing.T) {
	assert.Contains(t, placeholderReadHint(syscall.EDEADLK), "hydrate")
	assert.Contains(t, placeholderReadHint(&os.PathError{Err: syscall.EDEADLK}), "hydrate")
	assert.Empty(t, placeholderReadHint(errors.New("boom")))
	assert.Empty(t, placeholderReadHint(os.ErrPermission))
}
