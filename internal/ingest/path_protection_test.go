package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPathProtectionRejectsChangedRoots(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.Mkdir(path, 0o700))
	proof, err := CapturePathProtection([]string{path})
	require.NoError(t, err)
	require.NoError(t, proof.Check([]string{path}))
	require.ErrorContains(t, proof.Check([]string{path, t.TempDir()}), "protected roots changed")
	require.NoError(t, os.Rename(path, path+"-previous"))
	require.NoError(t, os.Mkdir(path, 0o700))
	require.ErrorContains(t, proof.Check([]string{path}), "changed during validation")
}
