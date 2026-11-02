//go:build !windows && !unix

package packstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenNoFollowFailsClosedOnFallbackPlatforms(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("target content"), 0o600))

	f, err := openNoFollow(target, false)
	if f != nil {
		require.NoError(t, f.Close())
	}
	require.ErrorContains(t, err, "unsupported platform")
}
