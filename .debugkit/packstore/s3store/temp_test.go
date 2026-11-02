package s3store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestCreatePrivateTempUsesValidatedUserDirectory(t *testing.T) {
	require := require.New(t)
	base := t.TempDir()
	file, err := createPrivateTempIn(base, "stage-*")
	require.NoError(err)
	t.Cleanup(func() {
		require.NoError(file.Close())
		require.NoError(os.Remove(file.Name()))
	})

	dir := filepath.Dir(file.Name())
	require.NoError(safefileio.ValidatePrivateDir(dir))
	assert.Equal(t, base, filepath.Dir(dir))
}
