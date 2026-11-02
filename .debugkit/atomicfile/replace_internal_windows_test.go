//go:build windows

package atomicfile

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// The MoveFileEx fallback alone cannot replace a target that a reader holds
// open with FILE_SHARE_DELETE; the POSIX-semantics rename in Replace can.
func TestReplacePOSIXSemanticsBeatMoveFileEx(t *testing.T) {
	dir := t.TempDir()
	src := writeTestFile(t, dir, "src", "new")
	dst := writeTestFile(t, dir, "dst", "old")
	p, err := windows.UTF16PtrFromString(dst)
	require.NoError(t, err)
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	held := os.NewFile(uintptr(h), dst)
	defer held.Close()

	err = moveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	assert.Equal(t, "old", readTestFile(t, dst))

	require.NoError(t, Replace(src, dst))
	assert.Equal(t, "new", readTestFile(t, dst))
	data, err := io.ReadAll(held)
	require.NoError(t, err)
	assert.Equal(t, "old", string(data))
}

func TestReplaceFallsBackOnlyWhenPOSIXRenameUnsupported(t *testing.T) {
	tests := []struct {
		err      windows.Errno
		fallback bool
	}{
		{windows.ERROR_INVALID_PARAMETER, true},
		{windows.ERROR_INVALID_FUNCTION, true},
		{windows.ERROR_NOT_SUPPORTED, true},
		{windows.ERROR_ACCESS_DENIED, false},
		{windows.ERROR_SHARING_VIOLATION, false},
	}
	for _, tt := range tests {
		t.Run(tt.err.Error(), func(t *testing.T) {
			orig := setRenameInfo
			t.Cleanup(func() { setRenameInfo = orig })
			setRenameInfo = func(windows.Handle, uint32, *byte, uint32) error { return tt.err }
			dir := t.TempDir()
			src := writeTestFile(t, dir, "src", "new")
			dst := writeTestFile(t, dir, "dst", "old")

			err := Replace(src, dst)

			if tt.fallback {
				require.NoError(t, err)
				assert.Equal(t, "new", readTestFile(t, dst))
				assert.NoFileExists(t, src)
				return
			}
			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, "new", readTestFile(t, src))
			assert.Equal(t, "old", readTestFile(t, dst))
		})
	}
}
