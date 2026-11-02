package renamebusy

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestReplaceStopsBeforeDelayExceedsBudget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "destination")
	require.NoError(t, os.WriteFile(src, []byte("original"), 0o600))
	handle, err := os.Open(src)
	require.NoError(t, err)

	synctest.Test(t, func(t *testing.T) {
		closed := make(chan error, 1)
		timer := time.AfterFunc(100*time.Millisecond, func() { closed <- handle.Close() })
		started := time.Now()
		err = Replace(src, dst, 20*time.Millisecond)
		elapsed := time.Since(started)
		if timer.Stop() {
			require.NoError(t, handle.Close())
		} else {
			require.NoError(t, <-closed)
		}
		require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
		assert.LessOrEqual(t, elapsed, 20*time.Millisecond)
	})
}
