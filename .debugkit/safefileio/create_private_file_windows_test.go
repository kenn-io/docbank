//go:build windows

package safefileio

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestCreatePrivateFileCreatesProtectedPrivateFile(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")

	file, err := CreatePrivateFile(path)
	require.NoError(err)
	defer func() { _ = file.Close() }()

	require.NoError(ValidatePrivateCurrentUserFile(file))
	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	require.NoError(err)
	control, _, err := descriptor.Control()
	require.NoError(err)
	assert.NotZero(t, control&windows.SE_DACL_PROTECTED)
	dacl, _, err := descriptor.DACL()
	require.NoError(err)
	require.NotNil(dacl)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(err)
	// The current user can also be LocalSystem; compare principals, not ACE counts.
	wantPrincipals := map[string]bool{
		user.User.Sid.String(): true,
		"S-1-5-18":             true, // LocalSystem
		"S-1-5-32-544":         true, // Built-in Administrators
	}
	principals := make(map[string]bool)
	for i := range dacl.AceCount {
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(windows.GetAce(dacl, uint32(i), &ace))
		principals[(*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()] = true
		assert.Zero(t, ace.Header.AceFlags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE|windows.INHERITED_ACE))
	}
	assert.Equal(t, wantPrincipals, principals)

	_, err = file.WriteString("{}")
	require.NoError(err)
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(err)
	data, err := io.ReadAll(file)
	require.NoError(err)
	assert.Equal(t, "{}", string(data))
}

func TestCreatePrivateFileRejectsExistingFile(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record.json")
	require.NoError(os.WriteFile(path, []byte("original"), 0o644))

	file, err := CreatePrivateFile(path)
	require.ErrorIs(err, fs.ErrExist)
	require.Nil(file)

	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, "original", string(data))
}

func TestCreatePrivateFileRejectsExistingSymlink(t *testing.T) {
	tests := []struct {
		name         string
		createTarget bool
	}{
		{name: "dangling", createTarget: false},
		{name: "to regular file", createTarget: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			target := filepath.Join(dir, "target")
			link := filepath.Join(dir, "link")
			if tt.createTarget {
				require.NoError(os.WriteFile(target, []byte("target"), 0o644))
			}
			if err := os.Symlink(target, link); err != nil {
				if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
					t.Skipf("symlink creation requires privilege: %v", err)
				}
				require.NoError(err)
			}

			file, err := CreatePrivateFile(link)
			require.ErrorIs(err, fs.ErrExist)
			require.Nil(file)

			linkInfo, err := os.Lstat(link)
			require.NoError(err)
			assert.NotZero(t, linkInfo.Mode()&os.ModeSymlink)
			if !tt.createTarget {
				_, err := os.Lstat(target)
				assert.ErrorIs(t, err, fs.ErrNotExist)
				return
			}
			data, err := os.ReadFile(target)
			require.NoError(err)
			assert.Equal(t, "target", string(data))
		})
	}
}

func TestCreatePrivateTempCreatesPrivateFileInDir(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()

	first, err := CreatePrivateTemp(dir, "rec-*.json")
	require.NoError(err)
	defer func() { _ = first.Close() }()
	second, err := CreatePrivateTemp(dir, "rec-*.json")
	require.NoError(err)
	defer func() { _ = second.Close() }()

	assert.NotEqual(t, first.Name(), second.Name())
	for _, file := range []*os.File{first, second} {
		assert.Equal(t, dir, filepath.Dir(file.Name()))
		assert.Regexp(t, `^rec-[0-9]+\.json$`, filepath.Base(file.Name()))
		assert.NoError(t, ValidatePrivateCurrentUserFile(file))
	}
}

func TestDiscardCreatedWindowsFileDeletesThroughHandle(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	moved := filepath.Join(dir, "moved.json")
	path16, err := windows.UTF16PtrFromString(path)
	require.NoError(err)
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.CREATE_NEW,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	require.NoError(err)
	require.NoError(os.Rename(path, moved))
	require.NoError(os.WriteFile(path, []byte("replacement"), 0o600))

	require.NoError(discardCreatedWindowsFile(path, handle))

	_, err = os.Lstat(moved)
	require.ErrorIs(err, fs.ErrNotExist)
	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, "replacement", string(data))
}
