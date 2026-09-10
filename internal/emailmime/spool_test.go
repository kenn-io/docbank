package emailmime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/home"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := home.CanonicalRoot(t.TempDir())
	require.NoError(t, err)
	return dir
}

type cancellingReader struct {
	cancel    context.CancelFunc
	remaining int
}

func (r *cancellingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	r.cancel()
	return n, nil
}

func TestDecodeIntegrityAndCancellationRemoveOwnedState(t *testing.T) {
	valid := sha256.Sum256([]byte("x"))
	for _, tc := range []struct {
		name string
		ctx  context.Context
		hash string
	}{{"mismatch", context.Background(), hex.EncodeToString(sha256.New().Sum(nil))}, {"cancelled", cancelledContext(t), hex.EncodeToString(valid[:])}} {
		t.Run(tc.name, func(t *testing.T) {
			parent := canonicalTempDir(t)
			_, err := Decode(tc.ctx, tc.hash, 1, &chunkReader{data: []byte("x"), size: 1}, parent)
			require.Error(t, err)
			entries, readErr := os.ReadDir(parent)
			require.NoError(t, readErr)
			assert.Empty(t, entries)
		})
	}
}

func TestDecodeSourceIOErrorReturnsNilResultAndRemovesOwnedState(t *testing.T) {
	parent := canonicalTempDir(t)
	sentinel := errors.New("synthetic source transport failure")
	result, err := Decode(t.Context(), strings.Repeat("0", 64), 1, &dataThenErrorReader{err: sentinel}, parent)
	assert.Nil(t, result)
	require.ErrorIs(t, err, sentinel)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func cancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestRecoverStaleRemovesOnlyMarkedOwnedDirectories(t *testing.T) {
	parent := canonicalTempDir(t)
	owned := filepath.Join(parent, "docbank-email-owned")
	require.NoError(t, os.Mkdir(owned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(owned, spoolMarkerName), []byte(spoolMarker), 0o600))
	unmarked := filepath.Join(parent, "docbank-email-unmarked")
	require.NoError(t, os.Mkdir(unmarked, 0o700))
	unrelated := filepath.Join(parent, "other")
	require.NoError(t, os.Mkdir(unrelated, 0o700))
	link := filepath.Join(parent, "docbank-email-link")
	require.NoError(t, os.Symlink(unrelated, link))
	count, err := RecoverStale(t.Context(), parent)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	_, err = os.Stat(owned)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, path := range []string{unmarked, unrelated, link} {
		_, err = os.Lstat(path)
		assert.NoError(t, err)
	}
}

func TestRecoverStaleBoundsMarkerBeforeRead(t *testing.T) {
	parent := canonicalTempDir(t)
	owned := filepath.Join(parent, "docbank-email-oversized-marker")
	require.NoError(t, os.Mkdir(owned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(owned, spoolMarkerName), []byte(spoolMarker+"extra"), 0o600))
	count, err := RecoverStale(t.Context(), parent)
	require.NoError(t, err)
	assert.Zero(t, count)
	_, err = os.Stat(owned)
	require.NoError(t, err)
	reader := &zeroReader{remaining: 1 << 20}
	matches, err := ownershipMarkerMatches(reader)
	require.NoError(t, err)
	assert.False(t, matches)
	assert.Equal(t, int64(len(spoolMarker)+1), reader.read)
}

func TestRecoverStaleRejectsSymlinkedRoot(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.Symlink(root, link))
	_, err := RecoverStale(t.Context(), link)
	require.Error(t, err)
}

func TestDecodeLiveCancellationCleansOwnedDirectory(t *testing.T) {
	parent := canonicalTempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancellingReader{cancel: cancel, remaining: 64 << 10}
	_, err := Decode(ctx, strings.Repeat("0", 64), 64<<10, reader, parent)
	require.ErrorIs(t, err, context.Canceled)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestResultArtifactCatalogIsDetachedAndCloseIsIdempotent(t *testing.T) {
	result := decodeFixture(t, "\r\nbody")
	first := result.Artifacts()
	require.NotEmpty(t, first)
	first[0].PartPath = "9"
	assert.NotEqual(t, "9", result.Artifacts()[0].PartPath)
	require.NoError(t, result.Close())
	require.NoError(t, result.Close())
	_, err := result.OpenArtifact(t.Context(), "1", "decoded_payload")
	require.Error(t, err)
}

func TestResultRejectsArtifactSymlinkReplacement(t *testing.T) {
	result := decodeFixture(t, "\r\nbody")
	var filename string
	for _, item := range result.artifacts {
		if item.artifact.PartPath == "1" && item.artifact.Reference.Role == "decoded_payload" {
			filename = item.filename
			break
		}
	}
	require.NotEmpty(t, filename)
	require.NoError(t, result.spool.root.Rename(filename, filename+"-original"))
	require.NoError(t, result.spool.root.Symlink(filename+"-original", filename))
	_, err := result.OpenArtifact(t.Context(), "1", "decoded_payload")
	require.ErrorContains(t, err, "symlink")
}
