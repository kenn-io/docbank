//go:build windows

package packstore

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestRetirePackWindowsSharingViolationIsRetryable(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	entry := buildStoreTestPack(t, layout, []byte("windows sharing retirement"))
	store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
		entry.Hash: {Member: true, Pack: &entry},
	}}, layout)
	stream, _, err := store.OpenStream(t.Context(), entry.Hash)
	require.NoError(err)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())

	name, err := windows.UTF16PtrFromString(layout.PackPath(entry.PackID))
	require.NoError(err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(err)
	err = store.RetirePack(entry.PackID)
	require.ErrorIs(err, ErrPackRetirementDeferred)
	require.NoError(windows.CloseHandle(handle))
	require.NoError(store.RetirePack(entry.PackID))
}
