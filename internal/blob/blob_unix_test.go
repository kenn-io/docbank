//go:build unix

package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAndExistsRefuseSymlinkedBlob(t *testing.T) {
	bs := newTestBlobStore(t)
	content := "real bytes"
	hash, _, err := bs.Write(strings.NewReader(content))
	require.NoError(t, err)

	// Swap the stored blob for a symlink to identical bytes elsewhere. The
	// read path must uphold the write path's regular-file invariant, not
	// serve whatever the link points at.
	sum := sha256.Sum256([]byte(content))
	require.Equal(t, hex.EncodeToString(sum[:]), hash)
	final := filepath.Join(bs.dir, hash[:2], hash)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(outside, []byte(content), 0o600))
	require.NoError(t, os.Remove(final))
	require.NoError(t, os.Symlink(outside, final))

	_, err = bs.Open(hash)
	require.Error(t, err)

	ok, err := bs.Exists(hash)
	require.Error(t, err)
	assert.False(t, ok, "a symlink at the blob path is not a stored blob")
}
