package emailmime

import (
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestSourceSpoolSharesOwnedRestartCleanup(t *testing.T) {
	parent := t.TempDir()
	source, err := NewSourceSpool(parent)
	require.NoError(t, err)
	_, err = source.WriteString("Synthetic source")
	require.NoError(t, err)
	_, err = source.Seek(0, 0)
	require.NoError(t, err)
	b := make([]byte, 9)
	_, err = source.Read(b)
	require.NoError(t, err)
	require.Equal(t, "Synthetic", string(b))
	require.NoError(t, source.Close())
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Empty(t, entries)
	source, err = NewSourceSpool(parent)
	require.NoError(t, err)
	// Simulate lost process handles, not normal cleanup.
	require.NoError(t, source.File.Close())
	require.NoError(t, source.spool.root.Close())
	source.spool.root = nil
	if source.spool.pin != nil {
		require.NoError(t, source.spool.pin.Close())
	}
	source.spool.pin = nil
	require.NoError(t, source.spool.parent.Close())
	source.spool.parent = nil
	removed, err := RecoverStale(t.Context(), parent)
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	entries, err = os.ReadDir(parent)
	require.NoError(t, err)
	require.Empty(t, entries)
}
