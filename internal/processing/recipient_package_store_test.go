package processing

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Lifecycle tests begin after verification; bytes are opaque to the store.
func readyRecipientPackage(t *testing.T) (*Service, *recipientPackageEntry) {
	t.Helper()
	f := newPreparationFixture(t, "unused")
	require.NoError(t, os.MkdirAll(f.service.spoolDirectory, 0o700))
	e, err := f.service.reserveRecipientPackage("alice")
	require.NoError(t, err)
	t.Cleanup(func() {
		if e.directory != "" {
			err := os.Chmod(e.directory, 0o700)
			if !os.IsNotExist(err) {
				require.NoError(t, err)
			}
		}
		e.pins = 0
		require.NoError(t, f.service.DiscardRecipientPackages(t.Context(), "alice"))
	})
	//nolint:usetesting // Exercise cleanup under the configured spool, as in production.
	e.directory, err = os.MkdirTemp(f.service.spoolDirectory, "native-pdf-")
	require.NoError(t, err)
	e.file, err = os.CreateTemp(e.directory, "recipient-*.zip")
	require.NoError(t, err)
	_, err = e.file.WriteString("verified archive")
	require.NoError(t, err)
	e.result.Size = 16
	e.result.ExpiresAt = f.service.clock().Add(recipientPackageLifetime)
	e.ready, e.pins = true, 0
	e.timer = time.NewTimer(time.Hour)
	return f.service, e
}

func TestRecipientPackageReaderLifetime(t *testing.T) {
	t.Parallel()
	s, e := readyRecipientPackage(t)
	r, err := s.AcquireRecipientPackage(t.Context(), "alice", e.result.ID)
	require.NoError(t, err)
	_, err = s.AcquireRecipientPackage(t.Context(), "alice", e.result.ID)
	require.ErrorIs(t, err, ErrRecipientPackageUnavailable)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "verified archive", string(data))
	require.NoError(t, r.Close())
	require.NoError(t, r.Close(), "closing twice must not release another pin")
	_, err = os.Stat(e.directory)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRecipientPackageExpiry(t *testing.T) {
	t.Parallel()
	s, e := readyRecipientPackage(t)
	s.clock = func() time.Time { return e.result.ExpiresAt }
	_, err := s.AcquireRecipientPackage(t.Context(), "alice", e.result.ID)
	require.ErrorIs(t, err, ErrRecipientPackageUnavailable)
	_, err = os.Stat(e.directory)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRecipientPackageReleaseStopsWaitingWhenCanceled(t *testing.T) {
	t.Parallel()
	s, e := readyRecipientPackage(t)
	reader, err := s.AcquireRecipientPackage(t.Context(), "alice", e.result.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	released := make(chan error, 1)
	go func() { released <- s.DiscardRecipientPackages(ctx, "alice") }()
	// Release cancels the reader first; a reader that has not closed keeps it waiting.
	<-reader.Context().Done()
	cancel()
	require.ErrorIs(t, <-released, context.Canceled, "an unfinished release is uncertain, not successful")
	_, err = os.Stat(e.directory)
	require.NoError(t, err, "files stay while the reader still holds them")
	require.NoError(t, reader.Close())
	_, err = os.Stat(e.directory)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRecipientPackageRemovalFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	s, e := readyRecipientPackage(t)
	reader, err := s.AcquireRecipientPackage(t.Context(), "alice", e.result.ID)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(e.directory, 0o500))
	probe := filepath.Join(e.directory, "permission-probe")
	if f, err := os.Create(probe); err == nil {
		require.NoError(t, f.Close())
		require.NoError(t, os.Remove(probe))
		t.Skip("platform does not enforce Unix directory permissions")
	}
	released := make(chan error, 1)
	go func() { released <- s.DiscardRecipientPackages(t.Context(), "alice") }()
	<-reader.Context().Done()
	require.ErrorIs(t, reader.Close(), fs.ErrPermission)
	require.ErrorIs(t, <-released, fs.ErrPermission, "release waits for the reader and reports its failed removal")
	require.ErrorIs(t, s.DiscardRecipientPackages(t.Context(), "alice"), fs.ErrPermission)
	files, err := os.ReadDir(e.directory)
	require.NoError(t, err)
	require.NotEmpty(t, files)
	_, err = s.reserveRecipientPackage("alice")
	require.ErrorIs(t, err, fs.ErrPermission)
	shutdown, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.Shutdown(shutdown), "failed deletion must not retain lifecycle registration")
	require.NoError(t, os.Chmod(e.directory, 0o700))
	require.NoError(t, s.DiscardRecipientPackages(t.Context(), "alice"))
	_, err = os.Stat(e.directory)
	require.ErrorIs(t, err, os.ErrNotExist)
}
