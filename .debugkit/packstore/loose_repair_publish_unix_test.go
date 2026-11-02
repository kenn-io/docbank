//go:build unix

package packstore

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplaceLooseRepairFileUnixPublishesAtomically(t *testing.T) {
	for _, tt := range []struct {
		name           string
		existingTarget bool
	}{
		{name: "replace existing", existingTarget: true},
		{name: "create absent"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			staging := filepath.Join(dir, "staging")
			final := filepath.Join(dir, "final")
			before := []byte("old open descriptor")
			after := []byte("verified replacement")
			require.NoError(os.WriteFile(staging, after, 0o600))
			verified, err := os.Stat(staging)
			require.NoError(err)
			var active *os.File
			if tt.existingTarget {
				require.NoError(os.WriteFile(final, before, 0o600))
				active, err = openNoFollow(final, false)
				require.NoError(err)
				t.Cleanup(func() { _ = active.Close() })
			}

			result, err := replaceLooseRepairFile(staging, final, verified)

			require.NoError(err)
			assert.True(result.Created)
			assert.False(result.KeepStaging)
			assert.True(result.SyncShard)
			assert.False(result.SyncStaging)
			assert.Equal(after, mustReadFile(t, final))
			assert.NoFileExists(staging)
			if active != nil {
				oldBytes, readErr := io.ReadAll(active)
				require.NoError(readErr)
				assert.Equal(before, oldBytes)
			}
		})
	}
}
