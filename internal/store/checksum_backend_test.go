package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChecksumRecordingSQLite(t *testing.T) {
	t.Parallel()
	checkChecksumRecording(t, newTestStore(t))
}

func TestChecksumRecordingPostgres(t *testing.T) {
	t.Parallel()
	checkChecksumRecording(t, newPostgresQueryStore(t))
}

func checkChecksumRecording(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const md5 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_, err := s.writeDB.ExecContext(ctx, `INSERT INTO blobs(hash,size,created_at) VALUES($1,17,$2)`, hash, "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	record := BlobChecksumRecord{BlobSHA256: hash, MD5: md5}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, s.RecordVerifiedBlobChecksum(cancelled, record), context.Canceled)
	_, err = s.BlobChecksums(ctx, hash)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, s.RecordVerifiedBlobChecksum(ctx, record))
	require.NoError(t, s.RecordVerifiedBlobChecksum(ctx, record))
	require.ErrorContains(t, s.RecordVerifiedBlobChecksum(ctx, BlobChecksumRecord{
		BlobSHA256: hash, MD5: "cccccccccccccccccccccccccccccccc",
	}), "different MD5")
	stored, err := s.BlobChecksums(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, record, stored)
	const missing = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	require.ErrorIs(t, s.RecordVerifiedBlobChecksum(ctx, BlobChecksumRecord{
		BlobSHA256: missing, MD5: md5,
	}), ErrNotFound)
	_, err = s.BlobChecksums(ctx, missing)
	require.ErrorIs(t, err, ErrNotFound)
}
