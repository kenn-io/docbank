package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareCreatesPrivateDetectedFileAndReleaseRemovesIt(t *testing.T) {
	content := testPDF("synthetic")
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	source := &observedReadCloser{Reader: bytes.NewReader(content)}
	policy := testPolicy(t, 1024, 10)

	prepared, err := Prepare(t.Context(), source, policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF,
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: 2048, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	assert.True(t, source.closed)
	assert.Equal(t, "pdf", prepared.Format().ID)
	assert.Equal(t, int64(len(content)), prepared.Size())
	assert.Equal(t, hex.EncodeToString(digest[:]), prepared.SHA256())
	assert.Equal(t, mediaTypePDF, prepared.MediaType())
	info, err := os.Lstat(prepared.path)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	path := prepared.path
	require.NoError(t, prepared.Release())
	assert.NoFileExists(t, path)
	require.NoError(t, prepared.Release())
	_, err = prepared.snapshot()
	require.ErrorContains(t, err, "released")
}

func TestReleaseWaitsForSpoolReservation(t *testing.T) {
	prepared := prepareTestDocument(t, testPolicy(t, 1024, 10), testPDF("release-lock"))
	releaseLock, err := acquireSpoolReservationLock(t.Context(), filepath.Dir(prepared.path))
	require.NoError(t, err)
	locked := true
	defer func() {
		if locked {
			releaseLock()
		}
	}()

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- prepared.Release()
	}()
	<-started
	select {
	case err := <-done:
		releaseLock()
		locked = false
		t.Fatalf("Release completed while the spool reservation was held: %v", err)
	case <-time.After(2 * spoolLockRetryInterval):
	}

	releaseLock()
	locked = false
	require.NoError(t, <-done)
}

func TestPrepareFailsClosedAndRemovesPartialFile(t *testing.T) {
	content := testPDF("synthetic")
	digest := sha256.Sum256(content)
	policy := testPolicy(t, 1024, 10)

	tests := []struct {
		name      string
		source    *observedReadCloser
		size      int64
		hash      string
		mediaType string
		wantError string
	}{
		{name: "hash", source: &observedReadCloser{Reader: bytes.NewReader(content)}, size: int64(len(content)), hash: stringsOfZero(64), mediaType: mediaTypePDF, wantError: "hash mismatch"},
		{name: "size", source: &observedReadCloser{Reader: bytes.NewReader(content)}, size: int64(len(content) + 1), hash: hex.EncodeToString(digest[:]), mediaType: mediaTypePDF, wantError: "size mismatch"},
		{name: "source exceeds reservation", source: &observedReadCloser{Reader: bytes.NewReader(content)}, size: int64(len(content) - 1), hash: hex.EncodeToString(digest[:]), mediaType: mediaTypePDF, wantError: "size mismatch"},
		{name: "close", source: &observedReadCloser{Reader: bytes.NewReader(content), closeErr: errors.New("synthetic close")}, size: int64(len(content)), hash: hex.EncodeToString(digest[:]), mediaType: mediaTypePDF, wantError: "close Mistral OCR source"},
		{name: "type", source: &observedReadCloser{Reader: bytes.NewReader(content)}, size: int64(len(content)), hash: hex.EncodeToString(digest[:]), mediaType: "text/plain", wantError: "not declared"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "spool")
			makePrivateDirectory(t, directory)
			_, err := Prepare(t.Context(), test.source, policy, PrepareOptions{
				Directory: directory, DeclaredMediaType: test.mediaType,
				ExpectedSize: test.size, ExpectedSHA256: test.hash,
				MaxSpoolBytes: 2048, MinFreeBytes: 1,
			})
			require.ErrorContains(t, err, test.wantError)
			assert.True(t, test.source.closed)
			requireOnlySpoolReservationFile(t, directory)
		})
	}
}

func TestPrepareDoesNotReadAgainAfterCopyFailure(t *testing.T) {
	copyErr := errors.New("synthetic copy failure")
	reads := 0
	source := &observedReadCloser{Reader: errorReader{err: copyErr, reads: &reads}}
	policy := testPolicy(t, 1024, 10)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)

	_, err := Prepare(t.Context(), source, policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF,
		ExpectedSize: 1, ExpectedSHA256: stringsOfZero(64),
		MaxSpoolBytes: 2048, MinFreeBytes: 1,
	})
	require.ErrorIs(t, err, copyErr)
	assert.Equal(t, 1, reads)
	assert.True(t, source.closed)
	requireOnlySpoolReservationFile(t, directory)
}

func TestPrepareDoesNotPublishAfterCancellationFollowingCopy(t *testing.T) {
	content := testPDF("cancel-after-copy")
	digest := sha256.Sum256(content)
	policy := testPolicy(t, 1024, 10)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	ctx, cancel := context.WithCancel(t.Context())
	source := &observedReadCloser{Reader: bytes.NewReader(content), onClose: cancel}

	prepared, err := Prepare(ctx, source, policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF,
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: 2048, MinFreeBytes: 1,
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, prepared)
	assert.True(t, source.closed)
	requireOnlySpoolReservationFile(t, directory)
}

func TestPrepareClassifiesCapacityRefusals(t *testing.T) {
	content := testPDF("synthetic")
	digest := sha256.Sum256(content)
	policy := testPolicy(t, 1024, 10)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "unrelated"), bytes.Repeat([]byte{'x'}, 1010), 0o600))
	options := PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF,
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: 1024, MinFreeBytes: 1,
	}
	_, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, options)
	require.ErrorIs(t, err, ErrSpoolCapacity)
	require.ErrorContains(t, err, "quota")

	options.MaxSpoolBytes = 2048
	options.MinFreeBytes = math.MaxInt64
	_, err = Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, options)
	require.ErrorIs(t, err, ErrSpoolCapacity)
	require.ErrorContains(t, err, "free-space reserve")

	options.MinFreeBytes = 1
	require.NoError(t, os.Mkdir(filepath.Join(directory, "unsafe"), 0o700))
	_, err = Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, options)
	require.ErrorContains(t, err, "unsafe entry")
	require.NotErrorIs(t, err, ErrSpoolCapacity)
}

func TestPrepareRejectsPublicReservationLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode-bit behavior")
	}
	content := testPDF("synthetic")
	digest := sha256.Sum256(content)
	policy := testPolicy(t, 1024, 10)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	lockPath := filepath.Join(directory, spoolReservationFile)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	require.NoError(t, os.Chmod(lockPath, 0o644))

	_, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF,
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.ErrorContains(t, err, "reservation lock")
}

func TestScavengeSpoolDirectoryRemovesOnlyStalePackageFilesAndFailsClosed(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	stale := filepath.Join(directory, spoolFilenamePrefix+"stale")
	live := filepath.Join(directory, spoolFilenamePrefix+"live")
	unrelated := filepath.Join(directory, "operator-owned")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	require.NoError(t, os.WriteFile(live, []byte("live"), 0o600))
	require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0o600))
	old := time.Now().UTC().Add(-3 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	removed, err := ScavengeSpoolDirectory(directory, time.Now().UTC().Add(-2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.NoFileExists(t, stale)
	assert.FileExists(t, live)
	assert.FileExists(t, unrelated)
	assert.FileExists(t, filepath.Join(directory, spoolReservationFile))

	require.NoError(t, os.Mkdir(filepath.Join(directory, "unsafe"), 0o700))
	_, err = ScavengeSpoolDirectory(directory, time.Now().UTC())
	require.ErrorContains(t, err, "unsafe entry")
	assert.FileExists(t, live)
	assert.FileExists(t, unrelated)
}
