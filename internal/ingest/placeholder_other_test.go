//go:build !darwin

package ingest

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Other platforms have no dataless-file concept: preflight reports zero
// placeholders and ingest adds no hint.
func TestCloudPlaceholderIsInertOffDarwin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.bin")
	require.NoError(t, os.WriteFile(path, []byte("bytes"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, isCloudPlaceholder(info))
	assert.Empty(t, placeholderReadHint(syscall.EDEADLK))
}
