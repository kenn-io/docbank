//go:build windows

package packstore

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestFilesystemBackendRetirementClassifiesWindowsSharingViolation(t *testing.T) {
	require := require.New(t)
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	entry := buildStoreTestPack(t, backend.Layout(), []byte("backend retirement sharing"))
	stream, _, err := backend.OpenPack(t.Context(), entry.Hash, entry)
	require.NoError(err)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())

	name, err := windows.UTF16PtrFromString(backend.Layout().PackPath(entry.PackID))
	require.NoError(err)
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	require.NoError(err)

	err = backend.Retire(t.Context(), ObjectRef{PackID: entry.PackID})
	require.ErrorIs(err, ErrPackRetirementDeferred)
	require.NoError(windows.CloseHandle(handle))
	require.NoError(backend.Retire(t.Context(), ObjectRef{PackID: entry.PackID}))
}
