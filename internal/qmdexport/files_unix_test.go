//go:build !windows

package qmdexport

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func broadenExportPermissions(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.Chmod(path, 0o777))
}
