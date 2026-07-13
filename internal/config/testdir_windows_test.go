//go:build windows

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func privateTestConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, safefileio.EnsurePrivateDir(dir))
	return dir
}
