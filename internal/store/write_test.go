package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHash returns a deterministic 64-char hex-looking hash for tests.
func fakeHash(seed string) string {
	return strings.Repeat("0", 64-len(seed)) + seed
}

func TestCreateFile(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	f, err := s.CreateFile(ctx, s.RootID(), "report.pdf", fakeHash("a1"), 1234, "application/pdf")
	require.NoError(t, err)
	assert.Equal(t, "file", f.Kind)
	assert.Equal(t, fakeHash("a1"), f.BlobHash)
	assert.Equal(t, int64(1234), f.Size)
	assert.Equal(t, "application/pdf", f.MimeType)

	// Blob row exists.
	var size int64
	require.NoError(t, s.db.QueryRow(
		`SELECT size FROM blobs WHERE hash = ?`, fakeHash("a1")).Scan(&size))
	assert.Equal(t, int64(1234), size)

	// Collision is strict.
	_, err = s.CreateFile(ctx, s.RootID(), "report.pdf", fakeHash("b2"), 99, "application/pdf")
	require.ErrorIs(t, err, ErrExists)

	// Same blob twice under different names: one blob row, two nodes.
	_, err = s.CreateFile(ctx, s.RootID(), "copy.pdf", fakeHash("a1"), 1234, "application/pdf")
	require.NoError(t, err)
	var blobCount int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&blobCount))
	assert.Equal(t, 1, blobCount)
}

func TestCreateFileRejectsFileParent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	f, err := s.CreateFile(ctx, s.RootID(), "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, f.ID, "b.txt", fakeHash("b2"), 1, "text/plain")
	assert.ErrorIs(t, err, ErrNotDir)
}
