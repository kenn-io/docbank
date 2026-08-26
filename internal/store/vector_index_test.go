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
		FencingToken: 1,
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
		FencingToken: 1,
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

	staleClaim, claimed, err := s.ClaimVectorIndexBuild(t.Context(), first.VectorSpace.ID,
		source.ManifestChecksum, "stale-worker", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	stale := vectorIndexGenerationFixture("stale", source, []byte("stale-generation"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), staleClaim, stale, now.Add(2*time.Minute)))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE nodes SET trashed_at=? WHERE current_version_id=?`,
			"2026-08-26T12:01:00.000000000Z", versionID)
		return err
	}))

	err = s.PublishVectorIndexGeneration(t.Context(), staleClaim, stale.ID, now.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrVectorIndexSourceStale)
	active, err := s.ActiveVectorIndexGeneration(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	assert.Equal(t, prior.ID, active.ID)
}

func TestVectorIndexReaderLeasePinsPriorGenerationAndExpiresWithFence(t *testing.T) {
	s, _, source := newPublishedVectorIndexFixture(t)
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	space := source.VectorSpaceID
	first := VectorIndexGenerationRecord{ID: hashVectorIndexTest("generation-a"),
		VectorSpaceID: space, SourceManifestChecksum: source.ManifestChecksum,
		IndexManifestChecksum: hashVectorIndexTest("manifest-a"), Bytes: []byte("generation-a"),
		RowCount: 1, BuiltAt: metadataEmbeddingTimeForTest(now)}
	second := first
	second.ID, second.SourceManifestChecksum, second.Bytes = hashVectorIndexTest("generation-b"), hashVectorIndexTest("source-b"), []byte("generation-b")
	require.NoError(t, putVectorIndexGenerationForTest(t, s, first))
	lease, err := s.AcquireVectorIndexGeneration(t.Context(), space, "reader", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, second))

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
	require.NoError(t, putVectorIndexGenerationForTest(t, s, record))
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
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
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
		FencingToken: 1,
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
	reclaimed, err := s.ReclaimVectorIndexGenerations(t.Context(), now.Add(30*time.Second))
	require.NoError(t, err)
	require.Zero(t, reclaimed, "a live builder owns its staged candidate")
	_, err = s.LoadVectorIndexGeneration(t.Context(), candidate.ID)
	require.NoError(t, err)

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

	layout, err := packstore.NewLayout(filepath.Join(filepath.Dir(s.path), "blobs"), packstore.LayoutOptions{
		Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
	})
	require.NoError(t, err)
	backend, err := packstore.NewFilesystemBackend(layout, packstore.FilesystemBackendOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	reader := &embeddingRestoreReader{store: s, backend: backend}
	_, err = s.ReadVectorIndexVectorSet(t.Context(), reader, member)
	require.ErrorIs(t, err, ErrVectorSetUnavailable)
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

	_, err = s.ReadVectorIndexVectorSet(t.Context(), reader, member)
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

func putVectorIndexGenerationForTest(t *testing.T, s *Store, record VectorIndexGenerationRecord) error {
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

func TestVectorIndexChangedMembershipSupersedesLiveBuildClaim(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	first := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), first))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: first.BindingID, InputKind: first.InputKind},
		SetID: first.ID, VectorSpaceID: first.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	source, err := s.CaptureVectorIndexSource(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	now := time.Now().UTC()
	old, claimed, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID, source.ManifestChecksum, "worker", now, 30*time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	_, claimed, err = s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID, source.ManifestChecksum, "competitor", now, time.Minute)
	require.NoError(t, err)
	require.False(t, claimed)
	secondProfile := embeddingCatalogProfileVariant(t)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { return ensureProcessingProfileTx(t.Context(), tx, secondProfile) }))
	second := embeddingSetFixture(s, versionID, secondProfile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	second.ID = testSHA256([]byte("second-profile-index-set"))
	second.InputGeneration.ID = testSHA256([]byte("second-profile-index-input"))
	require.NoError(t, s.StageEmbeddingSet(t.Context(), second))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: second.BindingID, InputKind: second.InputKind},
		SetID: second.ID, VectorSpaceID: second.VectorSpace.ID, ProcessingProfileFingerprint: secondProfile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	updated, err := s.CaptureVectorIndexSource(t.Context(), source.VectorSpaceID)
	require.NoError(t, err)
	next, claimed, err := s.ClaimVectorIndexBuild(t.Context(), updated.VectorSpaceID, updated.ManifestChecksum, "worker", now.Add(time.Second), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed, "new membership must not wait for the old build lease")
	require.Greater(t, next.FencingToken, old.FencingToken)
	require.NoError(t, s.AbandonVectorIndexBuild(t.Context(), old, now.Add(2*time.Second)))
	candidate := vectorIndexGenerationFixture("updated", updated, []byte("new"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), next, candidate, now.Add(2*time.Second)))
	require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), next, candidate.ID, now.Add(2*time.Second)))
	afterPublish, claimed, err := s.ClaimVectorIndexBuild(t.Context(), updated.VectorSpaceID, updated.ManifestChecksum, "worker", now.Add(3*time.Second), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Greater(t, afterPublish.FencingToken, next.FencingToken, "publication must preserve fencing history")
	require.ErrorIs(t, s.StageVectorIndexGeneration(t.Context(), old, vectorIndexGenerationFixture("obsolete", source, []byte("old"), now), now.Add(time.Second)), ErrVectorIndexBuildFenced)
	_, _, err = s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID, source.ManifestChecksum, "outdated-capture", now.Add(2*time.Second), time.Minute)
	require.ErrorIs(t, err, ErrVectorIndexSourceStale)
}

func newPublishedVectorIndexFixture(t *testing.T) (*Store, EmbeddingSetRecord, VectorIndexSource) {
	t.Helper()
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	return s, record, source
}

func TestVectorIndexAcquisitionRejectsTrashedMembership(t *testing.T) {
	s, record, source := newPublishedVectorIndexFixture(t)
	now := time.Now().UTC()
	generation := vectorIndexGenerationFixture("active", source, []byte("index"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
	_, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "before-trash", now, time.Minute)
	require.NoError(t, err)
	var nodeID int64
	require.NoError(t, s.db.QueryRow(`SELECT id FROM nodes WHERE current_version_id=?`, record.ContentVersionID).Scan(&nodeID))
	_, _, err = s.Trash(t.Context(), nodeID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "after-trash", now, time.Minute)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestVectorIndexSweepIncludesSpacesAfterLastEmbeddingHeadIsRemoved(t *testing.T) {
	s, _, source := newPublishedVectorIndexFixture(t)
	generation := vectorIndexGenerationFixture("active", source, []byte("index"), time.Now().UTC())
	require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM embedding_heads`)
		return err
	}))
	spaces, err := s.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Contains(t, spaces, source.VectorSpaceID)
}

func TestVectorIndexRetirementPreservesReadersUntilLeaseRelease(t *testing.T) {
	s, _, source := newPublishedVectorIndexFixture(t)
	now := time.Now().UTC()
	generation := vectorIndexGenerationFixture("retiring", source, []byte("index"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
	lease, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "reader", now, time.Minute)
	require.NoError(t, err)
	require.ErrorIs(t, s.RetireEmptyVectorIndexHead(t.Context(), source.VectorSpaceID, now), ErrVectorIndexSourceStale)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM embedding_heads`)
		return err
	}))
	require.NoError(t, s.RetireEmptyVectorIndexHead(t.Context(), source.VectorSpaceID, now))
	_, err = s.ActiveVectorIndexGeneration(t.Context(), source.VectorSpaceID)
	require.ErrorIs(t, err, ErrNotFound)
	removed, err := s.ReclaimVectorIndexGenerations(t.Context(), now)
	require.NoError(t, err)
	require.Zero(t, removed)
	require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, now))
	removed, err = s.ReclaimVectorIndexGenerations(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 1, removed)
}
