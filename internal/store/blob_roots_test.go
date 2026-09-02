package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every consumer of blob reachability must agree on one rule: GC candidates,
// the derivative purge inventory, and version-prune reference counts all read
// blobRootReferences. This fixture holds one blob per root kind, one blob per
// GC hold, and one orphan, and checks each consumer against the same answer.
func TestBlobReachabilityRuleAgreesAcrossConsumers(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	build := catalogRenditionBuild(s, catalogProcessingProfile(t, false))
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	previewOutput := fakeHash("71")
	_, err := s.PublishVisualPreview(ctx, versions[0],
		readyVisualPreview(t, catalogSourceHash, previewOutput, 9),
		&BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)
	staged, orphan, purgePending := fakeHash("72"), fakeHash("73"), fakeHash("74")
	require.NoError(t, s.RecordRenditionBlob(ctx, staged, 5,
		BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 5, Created: true}))
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := s.EnsureBlobTx(tx, orphan, 3); err != nil {
			return err
		}
		if err := s.EnsureBlobTx(tx, purgePending, 4); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO derivative_blob_purge_pending(blob_hash) VALUES(?)`, purgePending)
		return err
	}))

	unreachable, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{orphan}, blobHashes(unreachable),
		"only the orphan is collectible; roots and holds keep every other blob")
	page, err := s.UnreachableBlobsPageFrom(ctx, nil, 100)
	require.NoError(t, err)
	pageHashes := make([]string, 0, len(page.Items))
	for _, candidate := range page.Items {
		pageHashes = append(pageHashes, candidate.Hash)
	}
	assert.Equal(t, []string{orphan}, pageHashes)

	references := make(map[string]int)
	rows, err := s.db.QueryContext(ctx, `SELECT blob_hash, COUNT(*) FROM (
		`+blobReferenceRowsSQL(blobRootReferences)+`
		) GROUP BY blob_hash`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var hash string
		var count int
		require.NoError(t, rows.Scan(&hash, &count))
		references[hash] = count
	}
	require.NoError(t, rows.Err())
	for _, rooted := range []string{
		catalogSourceHash, catalogEvidenceBlobHash, catalogMarkdownBlobHash, previewOutput,
	} {
		assert.Positive(t, references[rooted], "%s must count as referenced", rooted)
	}
	for _, unrooted := range []string{staged, orphan, purgePending} {
		assert.Zero(t, references[unrooted], "%s must not count as referenced", unrooted)
	}

	purgeTargets, err := s.UnreachableDerivativePurgeBlobs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{purgePending}, blobHashes(purgeTargets))

	_, err = s.db.ExecContext(ctx, `DELETE FROM rendition_blob_staging WHERE blob_hash=?`, staged)
	require.NoError(t, err)
	unreachable, err = s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{staged, orphan}, blobHashes(unreachable),
		"staged bytes become collectible once no manifest claims them")
}

func blobHashes(blobs []BlobInfo) []string {
	hashes := make([]string, 0, len(blobs))
	for _, blob := range blobs {
		hashes = append(hashes, blob.Hash)
	}
	return hashes
}
