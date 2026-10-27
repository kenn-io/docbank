package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
)

func TestRedactionArchiveSnapshot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	data := []byte("original archive bytes")
	snapshot, err := snapshotRedactionArchive(t.Context(), bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = snapshot.Close() })
	clear(data)
	got, err := io.ReadAll(snapshot.file)
	require.NoError(t, err)
	require.Equal(t, "original archive bytes", string(got))
	require.Equal(t, redactionFixtureDigest(got).SHA256, snapshot.sha256)
	entries, err := os.ReadDir(snapshot.dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	info, err := snapshot.file.Stat()
	require.NoError(t, err)
	require.EqualValues(t, len(got), info.Size())
	_, err = snapshot.file.Write([]byte("changed"))
	require.Error(t, err)
	require.NoError(t, snapshot.Close())
	_, err = os.Stat(snapshot.dir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRedactionArchiveSnapshotFailures(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	failure := errors.New("synthetic archive read failure")
	for _, test := range []struct {
		name   string
		reader io.Reader
		size   int64
		want   error
	}{
		{"read", snapshotErrorReader{failure}, 1, failure},
		{"short", bytes.NewReader(nil), 1, daemonconn.ErrIntegrity},
		{"long", bytes.NewReader([]byte("ab")), 1, daemonconn.ErrIntegrity},
		{"oversized", snapshotErrorReader{failure}, processing.MaxRedactionArchiveBytes + 1, daemonconn.ErrIntegrity},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := snapshotRedactionArchive(t.Context(), test.reader, test.size)
			require.ErrorIs(t, err, test.want)
			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	input := snapshotCancelReader{cancel: cancel, Reader: bytes.NewReader([]byte("abc"))}
	_, err := snapshotRedactionArchive(ctx, input, 3)
	require.ErrorIs(t, err, context.Canceled)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
	// Failure to create the private directory is an I/O error, not corrupted input.
	missing := filepath.Join(root, "missing")
	t.Setenv("TMPDIR", missing)
	t.Setenv("TMP", missing)
	t.Setenv("TEMP", missing)
	_, err = snapshotRedactionArchive(t.Context(), bytes.NewReader([]byte("a")), 1)
	require.Error(t, err)
	require.NotErrorIs(t, err, daemonconn.ErrIntegrity)
}

type snapshotErrorReader struct{ err error }

func (r snapshotErrorReader) Read([]byte) (int, error) { return 0, r.err }

type snapshotCancelReader struct {
	io.Reader

	cancel context.CancelFunc
}

func (r snapshotCancelReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.cancel()
	return n, err
}

func TestRedactionVerifyErrorClasses(t *testing.T) {
	memory := &pdfproduction.RSSLimitError{ObservedBytes: 2, LimitBytes: 1}
	err := redactionCLIError(memory)
	require.ErrorIs(t, err, memory)
	require.Equal(t, 5, commandExitCode(err, true))
	require.Equal(t, 6, commandExitCode(redactionCLIError(pdfproduction.ErrPDFIntegrity), true))
	failure := errors.New("synthetic local I/O failure")
	err = redactionCLIError(failure)
	require.ErrorIs(t, err, failure)
	require.Equal(t, 1, commandExitCode(err, true))
}

func TestRedactionVerificationUsesOnlySnapshot(t *testing.T) {
	f := newRedactionVerifyFixture(t)
	original := f.archive(t, "valid")
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	input, err := os.Open(f.args[5])
	require.NoError(t, err)
	snapshot, err := snapshotRedactionArchive(t.Context(), input, int64(len(original)))
	require.NoError(t, input.Close())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, snapshot.Close()) })
	// Later changes to the caller's file cannot change any check or the digest.
	require.NoError(t, os.WriteFile(f.args[5], []byte("replaced original"), 0600))
	contents, err := daemonconn.VerifyRedactionArchive(t.Context(), snapshot.file, snapshot.size, f.review)
	require.NoError(t, err)
	reads := 0
	reader := snapshotObservedReader{ReaderAt: contents.PDF, observe: func() {
		reads++
		// Check on actual reads, including PDFium's OpenDocument callbacks. A
		// freezeFresh copy would already exist when it reads this source.
		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, filepath.Base(snapshot.dir), entries[0].Name())
		entries, err = os.ReadDir(snapshot.dir)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, "archive.zip", entries[0].Name())
	}}
	require.NoError(t, pdfproduction.VerifySaved(t.Context(), reader, contents.PDF.Size(), f.review.Resolved, f.review.Recipe, nil))
	require.Positive(t, reads)
	require.Equal(t, redactionFixtureDigest(original).SHA256, snapshot.sha256)
	info, err := snapshot.file.Stat()
	require.NoError(t, err)
	require.EqualValues(t, len(original), info.Size())
	t.Logf("single archive snapshot: %d bytes; %d PDF reader callbacks", info.Size(), reads)
}

type snapshotObservedReader struct {
	io.ReaderAt

	observe func()
}

func (r snapshotObservedReader) ReadAt(p []byte, offset int64) (int, error) {
	r.observe()
	return r.ReaderAt.ReadAt(p, offset)
}
