package backupapp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

// Set DOCBANK_LARGE_OBJECT_TEST_BYTES=4294967297 to exercise admission beyond
// the old 4 GiB boundary. The default still crosses a real backup chunk boundary.
func TestLargeObjectBackupRoundTrip(t *testing.T) {
	size := int64(64<<20 + 1)
	if value := os.Getenv("DOCBANK_LARGE_OBJECT_TEST_BYTES"); value != "" {
		var err error
		size, err = strconv.ParseInt(value, 10, 64)
		require.NoError(t, err)
		require.Greater(t, size, int64(64<<20))
	}
	fixture := newArchiveFixture(t)
	digest := sha256.New()
	source := io.TeeReader(io.LimitReader(rand.NewChaCha8([32]byte{1}), size), digest)
	var hash string
	require.NoError(t, fixture.blobs.WithMutation(t.Context(), func() error {
		receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), source)
		if err != nil {
			return err
		}
		hash = receipt.Hash
		assert.Equal(t, size, receipt.Size)
		assert.False(t, receipt.PackEligible)
		_, err = fixture.metadata.CreateFile(t.Context(), fixture.metadata.RootID(), "large.bin", hash, size, "application/octet-stream")
		return err
	}))
	wantHash := hex.EncodeToString(digest.Sum(nil))
	require.Equal(t, wantHash, hash)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(t.Context(), repo, "test", fixture.metadata, fixture.blobs, backup.CreateOptions{Jobs: 1})
	require.NoError(t, err)
	require.NotEmpty(t, manifest.Attachments.Recipes)
	verified, err := backup.Verify(t.Context(), repo, backupapp.New("test"), backup.VerifyOptions{Jobs: 1})
	require.NoError(t, err)
	require.Empty(t, verified.Problems)
	target := filepath.Join(t.TempDir(), "restored")
	result, err := backupapp.Restore(t.Context(), repo, "test", backup.RestoreOptions{TargetDir: target, Jobs: 1})
	require.NoError(t, err)
	assert.Positive(t, result.PackedAttachmentBlobs, "small fixture files still restore into managed packs")
	assert.Equal(t, int64(1), result.LooseAttachmentBlobs, "the large logical object restores as one loose file")
	restored, err := store.OpenForRestore(filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(restored), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	stream, restoredSize, err := blobs.OpenStreamContext(t.Context(), hash)
	require.NoError(t, err)
	digest.Reset()
	n, err := io.Copy(digest, stream)
	require.NoError(t, err)
	assert.True(t, stream.Verified())
	require.NoError(t, stream.Close())
	assert.Equal(t, size, restoredSize)
	assert.Equal(t, size, n)
	assert.Equal(t, wantHash, hex.EncodeToString(digest.Sum(nil)))
}
