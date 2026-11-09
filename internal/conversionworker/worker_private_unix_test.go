//go:build !windows

package conversionworker

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkerRefusesUnrelatedDirectoryWithoutChangingPermissions(t *testing.T) {
	o := workerOptions(t)
	require.NoError(t, os.Mkdir(o.Directory, 0755))
	require.NoError(t, os.Chmod(o.Directory, 0755))
	sentinel := filepath.Join(o.Directory, "unrelated")
	require.NoError(t, os.WriteFile(sentinel, []byte("synthetic unrelated bytes"), 0644))
	w, err := Open(t.Context(), o)
	if w != nil {
		require.NoError(t, w.Close(t.Context()))
	}
	require.Error(t, err)
	info, err := os.Stat(o.Directory)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0755), info.Mode().Perm())
	data, err := os.ReadFile(sentinel)
	require.NoError(t, err)
	require.Equal(t, "synthetic unrelated bytes", string(data))
}
