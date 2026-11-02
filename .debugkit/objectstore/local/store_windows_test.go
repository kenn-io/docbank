package local

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fsname"
	"golang.org/x/sys/windows"
)

func TestRejectActiveStagingShortName(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	data := []byte("original")
	source := &stagingPausedSource{Reader: bytes.NewReader(data), ready: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	resume := sync.OnceFunc(func() { close(source.resume) })
	go func() {
		_, err := store.Publish(t.Context(), "final", source, 8, sha256.Sum256(data))
		done <- err
	}()
	defer func() { resume(); assert.NoError(t, <-done) }()
	select {
	case <-source.ready:
	case err := <-done:
		done <- err
		require.NoError(t, err)
		require.FailNow(t, "publication completed before staging pause")
	}
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	long, err := windows.UTF16PtrFromString(filepath.Join(root, entries[0].Name()))
	require.NoError(t, err)
	buffer := make([]uint16, 32768)
	n, err := windows.GetShortPathName(long, &buffer[0], uint32(len(buffer)))
	require.NoError(t, err)
	require.Less(t, n, uint32(len(buffer)))
	alias := filepath.Base(windows.UTF16ToString(buffer[:n]))
	if strings.EqualFold(alias, entries[0].Name()) {
		t.Skip("volume does not create 8.3 aliases")
	}
	// Establish that this is the active file's alias, not an invented short name.
	staged, err := os.ReadFile(filepath.Join(root, alias))
	require.NoError(t, err)
	require.Equal(t, data, staged)
	_, err = store.Inspect(t.Context(), alias)
	require.ErrorIs(t, err, fsname.ErrNotPortable)
	body, err := store.OpenRange(t.Context(), alias, "", 0, 8)
	if body != nil {
		require.NoError(t, body.Close())
	}
	require.ErrorIs(t, err, fsname.ErrNotPortable)
	receipt, err := store.Publish(t.Context(), alias, bytes.NewReader(data), 8, sha256.Sum256(data))
	require.ErrorIs(t, err, fsname.ErrNotPortable)
	assert.Zero(t, receipt)
	require.ErrorIs(t, store.Delete(t.Context(), alias, ""), fsname.ErrNotPortable)
}
