//go:build windows

package telemetry

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func requirePrivateInstallFile(t *testing.T, path string) {
	t.Helper()
	file, err := safefileio.OpenCurrentUserFile(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	require.NoError(t, safefileio.ValidatePrivateCurrentUserFile(file))
}
