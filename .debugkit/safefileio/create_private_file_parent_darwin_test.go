package safefileio

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscardCreatedFileLeavesFileWhenParentHasExtendedACL(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	output, err := exec.CommandContext(t.Context(),
		"chmod", "+a", "everyone allow add_file,delete_child", dir,
	).CombinedOutput()
	require.NoError(err, string(output))
	path := filepath.Join(dir, "record.json")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(err)

	require.ErrorContains(discardCreatedFile(path, file), "extended ACL")

	_, err = os.Lstat(path)
	require.NoError(err)
}
