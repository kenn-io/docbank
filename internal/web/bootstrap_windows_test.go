//go:build windows

package web

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"

	"go.kenn.io/docbank/internal/winsecurity"
)

func TestWriteBootstrapAppliesRestrictedWindowsDACL(t *testing.T) {
	root := t.TempDir()
	_, err := WriteBootstrap(root, "http://127.0.0.1:43210/#web_session=private")
	require.NoError(t, err)
	dir := filepath.Join(root, launchDirName)
	require.NoError(t, safefileio.ValidatePrivateDir(dir))
	file, err := winsecurity.OpenRestrictedCurrentUserFile(filepath.Join(dir, "index.html"))
	require.NoError(t, err)
	require.NoError(t, file.Close())
}
