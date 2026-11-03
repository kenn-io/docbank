package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/packstore"
)

func TestVectorIndexSourceCapturesExactEligibleMembership(t *testing.T) {
	t.Parallel()
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(t.Context(), s, versionID, profile.Fingerprint,
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
	t.Parallel()
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	first := embeddingSetFixture(t.Context(), s, versionID, profile.Fingerprint,
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
	t.Parallel()
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
	t.Parallel()
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
		require.NoError(t, restored.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count))
		assert.Zero(t, count, table)
	}
}

func TestVectorIndexCandidateCanBeRestagedAfterCrashedClaimExpires(t *testing.T) {
	t.Parallel()
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(t.Context(), s, versionID, profile.Fingerprint,
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
	t.Parallel()
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(t.Context(), s, versionID, profile.Fingerprint,
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

func putVectorIndexGenerationForTest(tb testing.TB, s *Store, record VectorIndexGenerationRecord) error {
	tb.Helper()
	return s.withStorageTx(tb.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(tb.Context(), `INSERT INTO vector_index_generations(
			generation_id,vector_space_id,source_manifest_checksum,index_manifest_checksum,
			generation_bytes,byte_size,row_count,built_at) VALUES(?,?,?,?,?,?,?,?)`, record.ID,
			record.VectorSpaceID, record.SourceManifestChecksum, record.IndexManifestChecksum,
			record.Bytes, len(record.Bytes), record.RowCount, record.BuiltAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(tb.Context(), `INSERT INTO vector_index_heads(
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
	t.Parallel()
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	first := embeddingSetFixture(t.Context(), s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
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
	second := embeddingSetFixture(t.Context(), s, versionID, secondProfile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
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

func newPublishedVectorIndexFixture(tb testing.TB) (*Store, EmbeddingSetRecord, VectorIndexSource) {
	tb.Helper()
	s, versionID, profile, _ := newEmbeddingCatalogFixture(tb)
	record := embeddingSetFixture(tb.Context(), s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(tb, s.StageEmbeddingSet(tb.Context(), record))
	require.NoError(tb, s.PublishEmbeddingHead(tb.Context(), EmbeddingHeadRecord{
		FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	source, err := s.CaptureVectorIndexSource(tb.Context(), record.VectorSpace.ID)
	require.NoError(tb, err)
	return s, record, source
}

func TestVectorIndexAcquisitionRejectsTrashedMembership(t *testing.T) {
	t.Parallel()
	s, record, source := newPublishedVectorIndexFixture(t)
	now := time.Now().UTC()
	generation := vectorIndexGenerationFixture("active", source, []byte("index"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
	_, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "before-trash", now, time.Minute)
	require.NoError(t, err)
	var nodeID int64
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT id FROM nodes WHERE current_version_id=?`, record.ContentVersionID).Scan(&nodeID))
	_, _, err = s.Trash(t.Context(), nodeID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "after-trash", now, time.Minute)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestVectorIndexSweepIncludesSpacesAfterLastEmbeddingHeadIsRemoved(t *testing.T) {
	t.Parallel()
	s, _, source := newPublishedVectorIndexFixture(t)
	generation := vectorIndexGenerationFixture("active", source, []byte("index"), time.Now().UTC())
	require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM embedding_heads`)
		return err
	}))
	spaces, err := s.ListVectorIndexSpaces(t.Context())
	require.NoError(t, err)
	require.Contains(t, spaces, source.VectorSpaceID)
}

func TestVectorIndexRetirementPreservesReadersUntilLeaseRelease(t *testing.T) {
	t.Parallel()
	s, _, source := newPublishedVectorIndexFixture(t)
	now := time.Now().UTC()
	generation := vectorIndexGenerationFixture("retiring", source, []byte("index"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
	lease, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "reader", now, time.Minute)
	require.NoError(t, err)
	require.ErrorIs(t, s.RetireEmptyVectorIndexHead(t.Context(), source.VectorSpaceID, now), ErrVectorIndexSourceStale)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM embedding_heads`)
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

func TestVectorIndexPayloadClassifiesBackendFailures(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		cause   error
		missing bool
	}{
		{"missing object", fmt.Errorf("remote object: %w", packstore.ErrPhysicalMissing), true},
		{"unavailable store", packstore.ErrStoreUnavailable, false},
		{"fenced store", packstore.ErrStoreFenced, false},
		{"corrupt object", packstore.ErrPhysicalCorrupt, false},
		{"missing and corrupt copies", errors.Join(packstore.ErrPhysicalMissing, packstore.ErrPhysicalCorrupt), false},
		{"missing and unavailable copies", errors.Join(packstore.ErrPhysicalMissing, packstore.ErrStoreUnavailable), false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			s, record, source := newPublishedVectorIndexFixture(t)
			reader := &embeddingRestoreReader{errorHash: record.VectorSet.PayloadBlobHash, readError: testCase.cause}
			_, err := s.ReadVectorIndexVectorSet(t.Context(), reader, source.Members[0])
			if testCase.missing {
				require.ErrorIs(t, err, ErrVectorSetUnavailable)
			} else {
				require.NotErrorIs(t, err, ErrVectorSetUnavailable)
				require.ErrorIs(t, err, testCase.cause)
			}
		})
	}
}

func TestVectorIndexAcquireRejectsHeadReplacedBeforeLease(t *testing.T) {
	t.Parallel()
	s, _, source := newPublishedVectorIndexFixture(t)
	now := time.Now().UTC()
	first := vectorIndexGenerationFixture("first", source, []byte("first"), now)
	second := vectorIndexGenerationFixture("second", source, []byte("second"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, first))
	require.NoError(t, putVectorIndexGenerationForTest(t, s, second))
	reclaimed, err := s.ReclaimVectorIndexGenerations(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)
	lease := VectorIndexReaderLease{ID: "reader-race", Generation: first, FencingToken: 1, ExpiresAt: now.Add(time.Minute)}
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return insertVectorIndexReaderLeaseTx(t.Context(), tx, lease, "reader")
	})
	require.ErrorIs(t, err, ErrVectorIndexSourceStale)
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_index_reader_leases WHERE lease_id=?`, lease.ID).Scan(&count))
	require.Zero(t, count)
}

func TestVectorIndexSourceCaptureMatchesPerMemberRule(t *testing.T) {
	t.Parallel()
	f, image := newMixedVectorIndexFixture(t)
	s := f.s
	var err error
	space := image.VectorSpace.ID
	publish := func(record EmbeddingSetRecord) EmbeddingSetRecord {
		record.VectorSpace.Descriptor = f.request.Descriptor
		require.Equal(t, space, record.VectorSpace.ID)
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: record.ContentVersionID, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: record.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
		}))
		return record
	}
	check := func(name string, want ...string) {
		t.Run(name, func(t *testing.T) {
			got, err := s.CaptureVectorIndexSource(t.Context(), space)
			if len(want) == 0 {
				require.ErrorIs(t, err, ErrNotFound)
				return
			}
			require.NoError(t, err)
			slices.Sort(want)
			var ids []string
			for _, member := range got.Members {
				ids = append(ids, member.EmbeddingSetID)
			}
			require.Equal(t, want, ids)
		})
	}
	originals := make([]EmbeddingSetRecord, 3)
	for i := range originals {
		sourceHash := hashVectorIndexTest(fmt.Sprintf("source-%d", i))
		if i == 0 {
			sourceHash = catalogSourceHash
		}
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("source-%d.pdf", i), sourceHash, 20, "application/pdf")
		require.NoError(t, err)
		originals[i] = publish(embeddingSetFixture(t.Context(), s, node.CurrentVersionID, f.request.Profile.Fingerprint, document.EmbeddingInputOriginalFile, "required", ""))
		if i == 1 {
			_, _, err = s.Trash(t.Context(), node.ID, UnconditionalRev)
			require.NoError(t, err)
		}
		if i == 2 {
			_, _, err = s.ReplaceContent(t.Context(), node.ID, UnconditionalRev, hashVectorIndexTest("replacement"), 20, "application/pdf")
			require.NoError(t, err)
		}
	}
	check("live original with trashed and replaced heads", image.ID, originals[0].ID)

	build := catalogRenditionBuild(s, f.request.Profile)
	build.EvidenceChecksum = embeddingCatalogEvidence(t).Checksum
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{ID: hashVectorIndexTest("capture-attachment"), VaultID: s.VaultID(), ContentVersionID: originals[0].ContentVersionID, BuildID: build.ID, Profile: f.request.Profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, publishRenditionForTest(t, s, attachment, embeddingCatalogTime, hashVectorIndexTest("capture-lexical")))
	chunk := publish(embeddingSetFixture(t.Context(), s, attachment.ContentVersionID, f.request.Profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment.ID))
	check("current rendition attachment", image.ID, originals[0].ID, chunk.ID)

	photos := make([]EmbeddingSetRecord, 2)
	for i := range photos {
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("source-%d.jpg", i), hashVectorIndexTest(fmt.Sprintf("photo-%d", i)), 20, "image/jpeg")
		require.NoError(t, err)
		photos[i], _ = f.forNode(t, node).publish(t)
		if i == 0 {
			require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			_, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
			require.NoError(t, err)
		} else {
			_, _, err = s.Trash(t.Context(), node.ID, UnconditionalRev)
			require.NoError(t, err)
		}
	}
	check("mixed space with visible hidden and trashed photos", image.ID, originals[0].ID, chunk.ID)
	_, err = s.db.ExecContext(t.Context(), `DELETE FROM rendition_heads WHERE attachment_id=?`, attachment.ID)
	require.NoError(t, err)
	check("removed rendition head", image.ID, originals[0].ID)
	for _, version := range []string{image.ContentVersionID, originals[0].ContentVersionID} {
		content, err := s.ContentVersionByID(t.Context(), version)
		require.NoError(t, err)
		_, _, err = s.Trash(t.Context(), content.NodeID, UnconditionalRev)
		require.NoError(t, err)
	}
	check("nothing eligible")
}

func newMixedVectorIndexFixture(t *testing.T) (imageEmbeddingCatalogFixture, EmbeddingSetRecord) {
	t.Helper()
	f := newImageEmbeddingCatalogFixture(t, document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk, document.EmbeddingInputImage)
	image, _ := f.publish(t)
	return f, image
}

func TestVectorIndexSourceFenceRecapturesAfterMembershipWrites(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"head publish", "head replace", "head delete", "trash", "restore", "replace", "rendition publish", "rendition replace", "rendition delete", "photo hidden", "photo unhidden", "photo excluded", "display change", "preview delete", "ready preview", "photo file link", "photo file unlink"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, image := newMixedVectorIndexFixture(t)
			s := f.s
			exec := func(query string, args ...any) {
				t.Helper()
				require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), query, args...)
					return err
				}))
			}
			var version string
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT version_id FROM content_versions WHERE blob_hash=?`, catalogSourceHash).Scan(&version))
			original := embeddingSetFixture(t.Context(), s, version, f.request.Profile.Fingerprint, document.EmbeddingInputOriginalFile, "required", "")
			original.VectorSpace.Descriptor = f.request.Descriptor
			fencingToken := int64(1)
			publish := func(record EmbeddingSetRecord) {
				t.Helper()
				require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
					FencingToken: fencingToken, Key: EmbeddingHeadKey{ContentVersionID: record.ContentVersionID, BindingID: record.BindingID, InputKind: record.InputKind},
					SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: record.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
				}))
			}
			require.NoError(t, s.StageEmbeddingSet(t.Context(), original))
			publish(original)
			target := image
			var attachment RenditionAttachmentRecord
			if name == "rendition publish" || name == "rendition replace" || name == "rendition delete" {
				build := catalogRenditionBuild(s, f.request.Profile)
				build.EvidenceChecksum = embeddingCatalogEvidence(t).Checksum
				require.NoError(t, s.StageRenditionBuild(t.Context(), build))
				attachment = RenditionAttachmentRecord{ID: hashVectorIndexTest("fence-attachment"), VaultID: s.VaultID(), ContentVersionID: version, BuildID: build.ID, Profile: f.request.Profile, AttachedAt: embeddingCatalogTime}
				require.NoError(t, publishRenditionForTest(t, s, attachment, embeddingCatalogTime, hashVectorIndexTest("fence-lexical")))
				target = embeddingSetFixture(t.Context(), s, version, f.request.Profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment.ID)
				target.VectorSpace.Descriptor = f.request.Descriptor
				require.NoError(t, s.StageEmbeddingSet(t.Context(), target))
				publish(target)
			}
			if name == "head replace" {
				target = original
			}
			content, err := s.ContentVersionByID(t.Context(), target.ContentVersionID)
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(t.Context(), f.node.ID)
			require.NoError(t, err)
			var preview VisualPreviewGeneration
			switch name {
			case "head publish":
				exec(`DELETE FROM embedding_heads WHERE embedding_set_id=?`, target.ID)
			case "rendition publish":
				exec(`DELETE FROM rendition_heads WHERE attachment_id=?`, attachment.ID)
			case "restore":
				_, _, err = s.Trash(t.Context(), content.NodeID, UnconditionalRev)
				require.NoError(t, err)
			case "photo unhidden":
				require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
				asset, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
				require.NoError(t, err)
			case "ready preview":
				fingerprints, err := document.CurrentPhotoQualityFingerprints()
				require.NoError(t, err)
				require.NoError(t, s.withReadTx(t.Context(), func(tx *sql.Tx) error {
					var err error
					preview, err = visualPreviewGenerationByRecipeTx(t.Context(), tx, target.ContentVersionID, fingerprints.GridRecipe)
					return err
				}))
				exec(`DELETE FROM visual_preview_generations WHERE content_version_id=?`, target.ContentVersionID)
			case "photo file link":
				// Move only the link away; the display file remains the asset's selected file.
				other, err := s.CreateFile(t.Context(), s.RootID(), "link-target.bin", hashVectorIndexTest("link-target"), 20, "application/octet-stream")
				require.NoError(t, err)
				exec(`UPDATE photo_files SET node_id=? WHERE file_id=?`, other.ID, asset.DisplayFileID)
			}
			before, err := s.CaptureVectorIndexSource(t.Context(), image.VectorSpace.ID)
			require.NoError(t, err)
			require.Equal(t, before.ManifestChecksum, memoVectorSourceChecksum(t, s, before.VectorSpaceID))
			switch name {
			case "head publish":
				publish(target)
			case "head replace":
				replacement := original
				replacement.ID = hashVectorIndexTest("replacement-set")
				replacement.InputGeneration.ID = hashVectorIndexTest("replacement-generation")
				require.NoError(t, s.StageEmbeddingSet(t.Context(), replacement))
				fencingToken++
				publish(replacement)
			case "head delete":
				exec(`DELETE FROM embedding_heads WHERE embedding_set_id=?`, target.ID)
			case "photo file unlink":
				exec(`DELETE FROM photo_files WHERE file_id=?`, asset.DisplayFileID)
			case "photo unhidden":
				token, _, unlockErr := s.UnlockPhotoHidden(t.Context(), "synthetic-passcode")
				require.NoError(t, unlockErr)
				_, err = s.SetPhotoAssetHidden(WithPhotoHiddenToken(t.Context(), token), asset.ID, asset.Revision, false)
			case "trash":
				_, _, err = s.Trash(t.Context(), content.NodeID, UnconditionalRev)
			case "restore":
				_, _, err = s.Restore(t.Context(), content.NodeID, UnconditionalRev)
			case "replace":
				_, _, err = s.ReplaceContent(t.Context(), content.NodeID, UnconditionalRev, hashVectorIndexTest("fence-replacement"), 20, "image/jpeg")
			case "rendition publish":
				err = publishRenditionForTest(t, s, attachment, embeddingCatalogTime, hashVectorIndexTest("fence-lexical"))
			case "rendition replace":
				build := catalogRenditionBuild(s, f.request.Profile)
				build.ID = hashVectorIndexTest("replacement-rendition-build")
				build.EvidenceChecksum = embeddingCatalogEvidence(t).Checksum
				require.NoError(t, s.StageRenditionBuild(t.Context(), build))
				attachment.ID = hashVectorIndexTest("replacement-rendition-attachment")
				attachment.BuildID = build.ID
				err = publishRenditionForTest(t, s, attachment, embeddingCatalogTime, hashVectorIndexTest("replacement-lexical"))
			case "rendition delete":
				exec(`DELETE FROM rendition_heads WHERE attachment_id=?`, attachment.ID)
			case "photo hidden":
				require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
				_, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
			case "photo excluded":
				_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
			case "display change":
				exec(`UPDATE photo_assets SET display_file_id=NULL WHERE asset_id=?`, asset.ID)
			case "preview delete":
				exec(`DELETE FROM visual_preview_generations WHERE content_version_id=?`, target.ContentVersionID)
			case "ready preview":
				_, err = s.PublishVisualPreview(t.Context(), target.ContentVersionID, preview.CanonicalResult, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
			case "photo file link":
				exec(`INSERT INTO photo_files(file_id,asset_id,node_id,role,created_at) VALUES(?,?,?,?,?)`, hashVectorIndexTest("new-photo-link"), asset.ID, f.node.ID, "primary", embeddingCatalogTime)
				exec(`UPDATE photo_assets SET display_file_id=? WHERE asset_id=?`, hashVectorIndexTest("new-photo-link"), asset.ID)
			}
			require.NoError(t, err)
			after, err := s.CaptureVectorIndexSource(t.Context(), before.VectorSpaceID)
			require.NoError(t, err)
			require.NotEqual(t, before.ManifestChecksum, after.ManifestChecksum)
			require.Equal(t, after.ManifestChecksum, memoVectorSourceChecksum(t, s, before.VectorSpaceID))
		})
	}
}

// memoVectorSourceChecksum exercises the same snapshot path as search checkpoints.
func memoVectorSourceChecksum(t *testing.T, s *Store, space string) string {
	t.Helper()
	var checksum string
	require.NoError(t, s.withReadTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		checksum, err = s.vectorIndexSourceChecksumTx(t.Context(), tx, space)
		return err
	}))
	return checksum
}

func countVectorSourceCaptures(t *testing.T, s *Store) *atomic.Int64 {
	t.Helper()
	var captures atomic.Int64
	release := make(chan struct{})
	close(release)
	readDB := s.db
	s.db = sql.OpenDB(photoSearchBeginConnector{db: readDB, start: func() {}, release: release, query: func(query string) {
		if strings.Contains(query, "JOIN embedding_vector_sets") {
			captures.Add(1)
		}
	}})
	t.Cleanup(func() {
		require.NoError(t, s.db.Close())
		require.NoError(t, readDB.Close())
	})
	return &captures
}

func TestVectorIndexSourceMemoSurvivesUnrelatedWrites(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	image, _ := f.publish(t)
	s := f.s
	source, err := s.CaptureVectorIndexSource(t.Context(), image.VectorSpace.ID)
	require.NoError(t, err)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, vectorIndexGenerationFixture("memo", source, []byte("index"), time.Now().UTC())))
	captures := countVectorSourceCaptures(t, s)
	require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, s, source.VectorSpaceID))
	require.EqualValues(t, 1, captures.Load())
	var version string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT version_id FROM content_versions WHERE blob_hash=?`, catalogSourceHash).Scan(&version))
	profile := embeddingCatalogProfile(t)
	other := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NotEqual(t, source.VectorSpaceID, other.VectorSpace.ID)
	asset, err := s.PhotoAssetForNode(t.Context(), f.node.ID)
	require.NoError(t, err)
	for _, name := range []string{"other space", "unembedded photo", "identical publication", "same display", "lease"} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "other space":
				require.NoError(t, s.StageEmbeddingSet(t.Context(), other))
				require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
					FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: other.ContentVersionID, BindingID: other.BindingID, InputKind: other.InputKind},
					SetID: other.ID, VectorSpaceID: other.VectorSpace.ID, ProcessingProfileFingerprint: other.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
				}))
			case "unembedded photo":
				_, err := s.CreateFile(t.Context(), s.RootID(), "unembedded.jpg", hashVectorIndexTest("unembedded-photo"), 20, "image/jpeg")
				require.NoError(t, err)
			case "identical publication":
				require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
					FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: image.ContentVersionID, BindingID: image.BindingID, InputKind: image.InputKind},
					SetID: image.ID, VectorSpaceID: image.VectorSpace.ID, ProcessingProfileFingerprint: image.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
				}))
			case "same display":
				_, err := s.SetPhotoDisplay(t.Context(), asset.ID, asset.Revision, asset.DisplayFileID)
				require.NoError(t, err)
			case "lease":
				lease, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "memo-reader", time.Now().UTC(), time.Minute)
				require.NoError(t, err)
				require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, time.Now().UTC()))
			}
			require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, s, source.VectorSpaceID))
			require.EqualValues(t, 1, captures.Load(), "unchanged membership must reuse the memo")
		})
	}
}

func TestVectorIndexSourceMemoRestartAndRecipe(t *testing.T) {
	t.Parallel()
	s, _, source := newPublishedVectorIndexFixture(t)
	require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, s, source.VectorSpaceID))
	reopened, err := Open(t.Context(), s.path, s.driver)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	captures := countVectorSourceCaptures(t, reopened)
	require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, reopened, source.VectorSpaceID))
	require.EqualValues(t, 1, captures.Load(), "a new Store starts with an empty memo")
	reopened.vectorSourceMu.Lock()
	memo := reopened.vectorSourceMemos[source.VectorSpaceID]
	memo.recipe = "old-recipe"
	reopened.vectorSourceMemos[source.VectorSpaceID] = memo
	reopened.vectorSourceMu.Unlock()
	require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, reopened, source.VectorSpaceID))
	require.EqualValues(t, 2, captures.Load(), "a changed recipe needs a fresh capture")
	require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, reopened, source.VectorSpaceID))
	require.EqualValues(t, 2, captures.Load())
}

func TestVectorIndexSourceMemoKeepsNewerSnapshot(t *testing.T) {
	t.Parallel()
	f, image := newMixedVectorIndexFixture(t)
	s := f.s
	var version string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT version_id FROM content_versions WHERE blob_hash=?`, catalogSourceHash).Scan(&version))
	record := embeddingSetFixture(t.Context(), s, version, f.request.Profile.Fingerprint, document.EmbeddingInputOriginalFile, "required", "")
	record.VectorSpace.Descriptor = f.request.Descriptor
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: record.BindingID, InputKind: record.InputKind},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: record.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
	}))
	before, err := s.CaptureVectorIndexSource(t.Context(), image.VectorSpace.ID)
	require.NoError(t, err)
	require.NoError(t, s.withReadTx(t.Context(), func(tx *sql.Tx) error {
		// Pin the old snapshot before the membership write and newer search.
		old, err := captureVectorIndexSourceTx(t.Context(), tx, image.VectorSpace.ID)
		require.NoError(t, err)
		require.Equal(t, before.ManifestChecksum, old.ManifestChecksum)
		_, _, err = s.Trash(t.Context(), f.node.ID, UnconditionalRev)
		require.NoError(t, err)
		after := memoVectorSourceChecksum(t, s, image.VectorSpace.ID)
		require.NotEqual(t, before.ManifestChecksum, after)
		checksum, err := s.vectorIndexSourceChecksumTx(t.Context(), tx, image.VectorSpace.ID)
		require.NoError(t, err)
		require.Equal(t, before.ManifestChecksum, checksum)
		captures := countVectorSourceCaptures(t, s)
		require.Equal(t, after, memoVectorSourceChecksum(t, s, image.VectorSpace.ID))
		require.Zero(t, captures.Load(), "an older snapshot must not replace the newer memo")
		return nil
	}))
}
