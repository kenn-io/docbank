package docbank

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/home"
)

func TestNewRecoversAbandonedEmailSpoolBeforeBlobCleanup(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	layout := home.Layout{Root: root}
	require.NoError(t, layout.Ensure())
	spool := filepath.Join(layout.BlobTmpDir(), "docbank-email-abandoned")
	require.NoError(t, os.Mkdir(spool, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(spool, ".docbank-email-spool"), []byte("docbank-email-spool/v1\n"), 0o600,
	))
	require.NoError(t, os.WriteFile(filepath.Join(spool, "source"), []byte("abandoned"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.BlobTmpDir(), "blob-unfinished"), []byte("partial"), 0o600,
	))

	v, err := New(t.Context(), Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, v.Close()) })
	entries, err := os.ReadDir(layout.BlobTmpDir())
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestVaultEmailKeepsExactSourceAndPart(t *testing.T) {
	v, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, v.Close()) })
	raw := []byte("Subject: Synthetic\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nouter-marker")
	created, err := v.Create(t.Context(), "/mail.eml", bytes.NewReader(raw), CreateOptions{
		MediaType: "message/rfc822", Expected: contentIdentity(raw),
	})
	require.NoError(t, err)
	pending, err := v.EmailMetadata(t.Context(), created.Version.ID)
	require.ErrorIs(t, err, ErrEmailPending)
	require.Equal(t, created.Version.ID, pending.Version.ID)
	view, err := v.EnsureEmailMetadata(t.Context(), created.Version.ID)
	require.NoError(t, err)
	require.Equal(t, created.Version.ID, view.Version.ID)
	current, err := v.EmailMetadata(t.Context(), created.Version.ID)
	require.NoError(t, err)
	require.Equal(t, view, current)
	exact, err := v.EmailMetadataGeneration(t.Context(), created.Version.ID, view.GenerationID)
	require.NoError(t, err)
	require.Equal(t, view, exact)
	stream, receipt, err := v.OpenEmailPart(
		t.Context(), created.Version.ID, view.GenerationID, "1", "decoded_payload",
	)
	require.NoError(t, err)
	payload, readErr := io.ReadAll(stream)
	closeErr := stream.Close()
	require.NoError(t, errors.Join(readErr, closeErr))
	require.Equal(t, []byte("outer-marker"), payload)
	require.Equal(t, int64(len(payload)), receipt.Size)
}

func TestVaultEnsureEmailMetadataPreservesUndeclaredSource(t *testing.T) {
	v, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, v.Close()) })
	raw := []byte("Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: base64\r\n\r\nbGVnYWN5LWNhZuk=")
	created, err := v.Create(t.Context(), "/mail.bin", bytes.NewReader(raw), CreateOptions{
		MediaType: "application/octet-stream", Expected: contentIdentity(raw),
	})
	require.NoError(t, err)
	view, err := v.EnsureEmailMetadata(t.Context(), created.Version.ID)
	require.NoError(t, err)
	require.Equal(t, "application/octet-stream", view.Version.MediaType)
	require.Equal(t, created.Version.BlobHash, view.Version.BlobHash)
}

func TestVaultOpenEmailPartHoldsLifecycleLease(t *testing.T) {
	v, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	raw := []byte("Content-Type: text/plain\r\n\r\nlease-body")
	created, err := v.Create(t.Context(), "/mail.eml", bytes.NewReader(raw), CreateOptions{
		MediaType: "message/rfc822", Expected: contentIdentity(raw),
	})
	require.NoError(t, err)
	view, err := v.EnsureEmailMetadata(t.Context(), created.Version.ID)
	require.NoError(t, err)
	stream, _, err := v.OpenEmailPart(t.Context(), created.Version.ID, view.GenerationID, "1", "decoded_payload")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, stream)
	require.NoError(t, err)

	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- v.Close()
	}()
	<-closeStarted
	select {
	case err := <-closeDone:
		require.FailNow(t, "vault closed while an email part held its lifecycle lease", "error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, stream.Close())
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "vault did not close after the email part released its lease")
	}
	_, _, err = v.OpenEmailPart(t.Context(), created.Version.ID, view.GenerationID, "1", "decoded_payload")
	require.ErrorIs(t, err, ErrClosed)
}

func TestVaultOpenEmailPartPhysicalFailureReleasesLifecycleLease(t *testing.T) {
	root := t.TempDir()
	v, err := New(t.Context(), Config{Root: root})
	require.NoError(t, err)
	raw := []byte("Content-Type: text/plain\r\n\r\nphysical-body")
	created, err := v.Create(t.Context(), "/mail.eml", bytes.NewReader(raw), CreateOptions{
		MediaType: "message/rfc822", Expected: contentIdentity(raw),
	})
	require.NoError(t, err)
	view, err := v.EnsureEmailMetadata(t.Context(), created.Version.ID)
	require.NoError(t, err)
	stream, receipt, err := v.OpenEmailPart(
		t.Context(), created.Version.ID, view.GenerationID, "1", "decoded_payload",
	)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "blobs", receipt.BlobSHA256[:2], receipt.BlobSHA256),
		[]byte("short"), 0o600,
	))

	_, failedReceipt, err := v.OpenEmailPart(
		t.Context(), created.Version.ID, view.GenerationID, "1", "decoded_payload",
	)
	require.ErrorIs(t, err, ErrContentUnavailable)
	require.Equal(t, receipt, failedReceipt)
	require.NoError(t, v.Close())
}
