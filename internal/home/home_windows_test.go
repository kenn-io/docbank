//go:build windows

package home

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestEnsureCreatesPrivateWindowsLayout(t *testing.T) {
	layout := Layout{Root: t.TempDir()}
	require.NoError(t, layout.Ensure())
	for _, dir := range []string{
		layout.Root, layout.BlobsDir(), layout.BlobTmpDir(), layout.LogsDir(),
	} {
		require.NoError(t, safefileio.ValidatePrivateDir(dir))
	}
	database, err := safefileio.OpenCurrentUserFile(layout.DBPath())
	require.NoError(t, err)
	require.NoError(t, database.Close())
}
