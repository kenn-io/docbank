package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/packstore"
)

func TestVectorIndexSourceCapturesExactEligibleMembership(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID,
			InputKind: record.InputKind},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	assert.Equal(t, record.VectorSpace.ID, source.VectorSpaceID)
	assert.Equal(t, []VectorIndexMember{{
		EmbeddingSetID: record.ID, VectorSetID: record.VectorSet.ID,
		PayloadBlobHash: record.VectorSet.PayloadBlobHash, PayloadSize: int64(len(record.VectorSet.Payload)),
	}}, source.Members)
	assert.Equal(t, vectorIndexSourceChecksum(source.Members), source.ManifestChecksum)

	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE nodes SET trashed_at=? WHERE current_version_id=?`,
			embeddingCatalogTime, versionID)
		return err
	}))
	_, err = s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestVectorIndexPublicationFencesMembershipDriftAndKeepsPriorHead(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	first := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), first))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: first.BindingID,
			InputKind: first.InputKind}, SetID: first.ID, VectorSpaceID: first.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	source, err := s.CaptureVectorIndexSource(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	claim, claimed, err := s.ClaimVectorIndexBuild(t.Context(), first.VectorSpace.ID,
		source.ManifestChecksum, "test-worker", now, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	prior := vectorIndexGenerationFixture("prior", source, []byte("prior-generation"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), claim, prior, now))
	require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), claim, prior.ID, now))

	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE nodes SET trashed_at=? WHERE current_version_id=?`,
			"2026-08-26T12:01:00.000000000Z", versionID)
		return err
	}))

	staleClaim, claimed, err := s.ClaimVectorIndexBuild(t.Context(), first.VectorSpace.ID,
		source.ManifestChecksum, "stale-worker", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	stale := vectorIndexGenerationFixture("stale", source, []byte("stale-generation"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), staleClaim, stale, now.Add(2*time.Minute)))
	err = s.PublishVectorIndexGeneration(t.Context(), staleClaim, stale.ID, now.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrVectorIndexSourceStale)
	active, err := s.ActiveVectorIndexGeneration(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	assert.Equal(t, prior.ID, active.ID)
}

func TestVectorIndexReaderLeasePinsPriorGenerationAndExpiresWithFence(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	space := hashVectorIndexTest("space")
	first := VectorIndexGenerationRecord{ID: hashVectorIndexTest("generation-a"),
		VectorSpaceID: space, SourceManifestChecksum: hashVectorIndexTest("source-a"),
		IndexManifestChecksum: hashVectorIndexTest("manifest-a"), Bytes: []byte("generation-a"),
		RowCount: 1, BuiltAt: metadataEmbeddingTimeForTest(now)}
	second := first
	second.ID, second.SourceManifestChecksum, second.Bytes = hashVectorIndexTest("generation-b"), hashVectorIndexTest("source-b"), []byte("generation-b")
	require.NoError(t, putActiveVectorIndexGenerationForTest(t, s, first))
	lease, err := s.AcquireVectorIndexGeneration(t.Context(), space, "reader", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, putActiveVectorIndexGenerationForTest(t, s, second))

	reclaimed, err := s.ReclaimVectorIndexGenerations(t.Context(), now.Add(30*time.Second))
	require.NoError(t, err)
	assert.Zero(t, reclaimed)
	assert.True(t, vectorIndexGenerationExistsForTest(t, s, first.ID))

	err = s.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, now.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrVectorIndexLeaseFenced)
	reclaimed, err = s.ReclaimVectorIndexGenerations(t.Context(), now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, reclaimed)
	assert.False(t, vectorIndexGenerationExistsForTest(t, s, first.ID))
}

func TestVectorIndexProjectionStateIsExcludedFromPortableMetadata(t *testing.T) {
	s := newTestStore(t)
	record := VectorIndexGenerationRecord{ID: hashVectorIndexTest("local-generation"),
		VectorSpaceID:          hashVectorIndexTest("local-space"),
		SourceManifestChecksum: hashVectorIndexTest("local-source"),
		IndexManifestChecksum:  hashVectorIndexTest("local-index"), Bytes: []byte("local-generation"),
		RowCount: 1, BuiltAt: "2026-08-26T13:00:00.000000000Z"}
	require.NoError(t, putActiveVectorIndexGenerationForTest(t, s, record))
	require.NoError(t, s.ReplaceVectorIndexUnavailableCoverage(t.Context(), []VectorIndexUnavailableCoverage{{
		VectorSpaceID: record.VectorSpaceID, SourceManifestChecksum: record.SourceManifestChecksum,
		Missing: []VectorIndexUnavailableSet{{EmbeddingSetID: hashVectorIndexTest("local-set"),
			VectorSetID: hashVectorIndexTest("local-vector-set"), PayloadBlobHash: hashVectorIndexTest("local-blob")}},
		ExternalReembeddingRequired: true,
	}}))

	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &metadata))
	assert.NotContains(t, metadata.String(), "vector_index_generation")
	assert.NotContains(t, metadata.String(), record.ID)

	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadataForRestore(t.Context(), bytes.NewReader(metadata.Bytes())))
	for _, table := range []string{"vector_index_generations", "vector_index_heads",
		"vector_index_build_jobs", "vector_index_reader_leases", "vector_index_unavailable_coverage"} {
		var count int
		require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&count))
		assert.Zero(t, count, table)
	}
}

func TestVectorIndexCandidateCanBeRestagedAfterCrashedClaimExpires(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID,
			InputKind: record.InputKind}, SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	now := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	firstClaim, claimed, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID,
		source.ManifestChecksum, "crashed-worker", now, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	candidate := vectorIndexGenerationFixture("crash-retry", source, []byte("same-candidate"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), firstClaim, candidate, now))

	secondClaim, claimed, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID,
		source.ManifestChecksum, "replacement-worker", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	retry := candidate
	retry.BuiltAt = metadataEmbeddingTimeForTest(now.Add(2 * time.Minute))
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), secondClaim, retry, now.Add(2*time.Minute)))
	stored, err := s.LoadVectorIndexGeneration(t.Context(), candidate.ID)
	require.NoError(t, err)
	assert.Equal(t, candidate.BuiltAt, stored.BuiltAt, "the first complete candidate remains immutable")
}

func TestVectorIndexPayloadDistinguishesMissingFromCorruptAuthority(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	member := VectorIndexMember{EmbeddingSetID: record.ID, VectorSetID: record.VectorSet.ID,
		PayloadBlobHash: record.VectorSet.PayloadBlobHash, PayloadSize: int64(len(record.VectorSet.Payload))}

	_, err := s.ReadVectorIndexVectorSet(t.Context(), member)
	require.ErrorIs(t, err, ErrVectorSetUnavailable)

	layout, err := packstore.NewLayout(filepath.Join(filepath.Dir(s.path), "blobs"), packstore.LayoutOptions{
		Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
	})
	require.NoError(t, err)
	loose, err := packstore.NewLooseStore(layout)
	require.NoError(t, err)
	hash, err := packstore.ParseHash(member.PayloadBlobHash)
	require.NoError(t, err)
	_, err = loose.WriteBytes(t.Context(), record.VectorSet.Payload, packstore.WriteOptions{
		Durability: packstore.AtomicPublication, Dedup: packstore.VerifyFullHash,
		ExpectedHash: hash, ExpectedSize: member.PayloadSize, SizeKnown: true,
	})
	require.NoError(t, err)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return writeLooseLocationTx(t.Context(), tx, s.primaryStoreID, member.PayloadBlobHash,
			BlobPhysical{Encoding: string(packstore.LooseEncodingRaw), StoredBytes: member.PayloadSize,
				PackEligible: true})
	}))
	require.NoError(t, os.WriteFile(layout.LoosePath(hash), bytes.Repeat([]byte{'x'}, int(member.PayloadSize)), 0o600))

	_, err = s.ReadVectorIndexVectorSet(t.Context(), member)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrVectorSetUnavailable)
}

func vectorIndexGenerationFixture(label string, source VectorIndexSource, data []byte, now time.Time) VectorIndexGenerationRecord {
	return VectorIndexGenerationRecord{ID: hashVectorIndexTest(label), VectorSpaceID: source.VectorSpaceID,
		SourceManifestChecksum: source.ManifestChecksum, IndexManifestChecksum: hashVectorIndexTest(label + "-manifest"),
		Bytes: data, RowCount: 1, BuiltAt: metadataEmbeddingTimeForTest(now)}
}

func hashVectorIndexTest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func metadataEmbeddingTimeForTest(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func putActiveVectorIndexGenerationForTest(t *testing.T, s *Store, record VectorIndexGenerationRecord) error {
	t.Helper()
	return s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO vector_index_generations(
			generation_id,vector_space_id,source_manifest_checksum,index_manifest_checksum,
			generation_bytes,byte_size,row_count,built_at) VALUES(?,?,?,?,?,?,?,?)`, record.ID,
			record.VectorSpaceID, record.SourceManifestChecksum, record.IndexManifestChecksum,
			record.Bytes, len(record.Bytes), record.RowCount, record.BuiltAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `INSERT INTO vector_index_heads(
			vector_space_id,generation_id,source_manifest_checksum) VALUES(?,?,?)
			ON CONFLICT(vector_space_id) DO UPDATE SET generation_id=excluded.generation_id,
			source_manifest_checksum=excluded.source_manifest_checksum`, record.VectorSpaceID,
			record.ID, record.SourceManifestChecksum)
		return err
	})
}

func vectorIndexGenerationExistsForTest(t *testing.T, s *Store, generationID string) bool {
	t.Helper()
	var present bool
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT EXISTS(
		SELECT 1 FROM vector_index_generations WHERE generation_id=?)`, generationID).Scan(&present))
	return present
}
