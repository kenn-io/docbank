package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"encoding/json/jsontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
	docsqlite "go.kenn.io/docbank/sqlite"
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

func TestVectorIndexSourceExcludesSuppressedSupersededAndReplacedAuthority(t *testing.T) {
	stageHead := func(t *testing.T, s *Store, versionID string, profile ProcessingProfileRecord,
		kind document.EmbeddingInputKind, bindingID, attachmentID string,
	) EmbeddingSetRecord {
		t.Helper()
		record := embeddingSetFixture(s, versionID, profile.Fingerprint, kind, bindingID, attachmentID)
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 1,
			Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: bindingID,
				InputKind: kind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
		return record
	}

	t.Run("suppression", func(t *testing.T) {
		s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
		record := stageHead(t, s, versionID, profile,
			document.EmbeddingInputOriginalFile, "optional", "")
		_, err := s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{versionID}})
		require.NoError(t, err)
		var heads, suppressions int
		require.NoError(t, s.db.QueryRow(`SELECT
			(SELECT COUNT(*) FROM embedding_heads WHERE content_version_id=?),
			(SELECT COUNT(*) FROM derivative_purge_suppressions WHERE active=1)`,
			versionID).Scan(&heads, &suppressions))
		require.Zero(t, heads)
		require.Positive(t, suppressions)
		_, err = s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("current version replacement", func(t *testing.T) {
		s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
		record := stageHead(t, s, versionID, profile,
			document.EmbeddingInputOriginalFile, "optional", "")
		var nodeID, revision int64
		require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`,
			versionID).Scan(&nodeID, &revision))
		replacement := hashVectorIndexTest("vector-index-current-version-replacement")
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			return s.EnsureBlobTx(tx, replacement, 24)
		}))
		_, _, err := s.ReplaceContent(t.Context(), nodeID, revision, replacement, 24, "application/pdf")
		require.NoError(t, err)
		var staleHeads int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_heads WHERE content_version_id=?`,
			versionID).Scan(&staleHeads))
		require.Equal(t, 1, staleHeads,
			"the source query, not eager head deletion, must fence a superseded version")
		_, err = s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("rendition attachment replacement", func(t *testing.T) {
		s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
		record := stageHead(t, s, versionID, profile,
			document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
		var buildID string
		require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`,
			attachmentID).Scan(&buildID))
		require.Equal(t, catalogBuildID, buildID)
		replacementBuild := cloneCatalogBuild(catalogRenditionBuild(s, profile))
		replacementBuild.ID = hashVectorIndexTest("vector-index-replacement-build")
		replacementBuild.ProviderOperationID = "synthetic-vector-index-replacement"
		const replacementPolicy = `{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"},{"max_count":2,"min_count":0,"role":"structured_evidence"}],"version":1}`
		replacementBuild.CapturedArtifactPolicy = jsontext.Value(replacementPolicy)
		replacementBuild.CapturedArtifactPolicyFingerprint = testSHA256([]byte(replacementPolicy))
		require.NoError(t, s.StageRenditionBuild(t.Context(), replacementBuild))
		replacement := RenditionAttachmentRecord{
			ID: hashVectorIndexTest("vector-index-replacement-attachment"), VaultID: s.VaultID(),
			ContentVersionID: versionID, BuildID: replacementBuild.ID, Profile: profile,
			AttachedAt: "2026-08-25T10:01:00.000000000Z",
		}
		require.NoError(t, publishRenditionForTest(t, s, replacement,
			"2026-08-25T10:02:00.000000000Z", hashVectorIndexTest("vector-index-replacement-lexical")))
		var staleHeads int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_heads WHERE embedding_set_id=?`,
			record.ID).Scan(&staleHeads))
		require.Zero(t, staleHeads,
			"publishing a replacement rendition attachment must revoke its old chunk head")
		_, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestVectorIndexGenerationIdentityChangesForMembershipOnlyDrift(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	first := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	second := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "required", "")
	require.Equal(t, first.VectorSpace.ID, second.VectorSpace.ID)
	require.Equal(t, first.VectorSet.ID, second.VectorSet.ID,
		"the regression requires membership-only drift over identical vector bytes")
	for _, record := range []EmbeddingSetRecord{first, second} {
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	}
	publish := func(record EmbeddingSetRecord, token int64) {
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: token,
			Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID,
				InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	publish(first, 1)
	firstSource, err := s.CaptureVectorIndexSource(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	set, _, err := document.DecodeVectorSetV1(first.VectorSet.Payload, document.VectorBounds{
		MaxRows: 10, MaxDimension: 8, MaxBytes: len(first.VectorSet.Payload),
	})
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{first.VectorSet.ID})
	require.NoError(t, err)
	built, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)

	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	stageAndPublish := func(source VectorIndexSource, at time.Time) VectorIndexGenerationRecord {
		claim, claimed, claimErr := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID,
			source.ManifestChecksum, "membership-test", at, time.Minute)
		require.NoError(t, claimErr)
		require.True(t, claimed)
		record := VectorIndexGenerationRecord{
			ID:            VectorIndexGenerationID(source.ManifestChecksum, built.Bytes()),
			VectorSpaceID: source.VectorSpaceID, SourceManifestChecksum: source.ManifestChecksum,
			IndexManifestChecksum: built.Metadata().Manifest.Checksum, Bytes: built.Bytes(),
			RowCount: built.Metadata().RowCount, BuiltAt: metadataEmbeddingTimeForTest(at),
		}
		require.NoError(t, s.StageVectorIndexGeneration(t.Context(), claim, record, at))
		require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), claim, record.ID, at))
		return record
	}
	firstGeneration := stageAndPublish(firstSource, now)

	publish(second, 1)
	secondSource, err := s.CaptureVectorIndexSource(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	require.NotEqual(t, firstSource.ManifestChecksum, secondSource.ManifestChecksum)
	require.Equal(t, manifest.Checksum, built.Metadata().Manifest.Checksum)
	secondGeneration := stageAndPublish(secondSource, now.Add(time.Minute))
	require.NotEqual(t, firstGeneration.ID, secondGeneration.ID)
	active, err := s.ActiveVectorIndexGeneration(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	require.Equal(t, secondGeneration.ID, active.ID)
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
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	embedding := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), embedding))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: embedding.BindingID,
			InputKind: embedding.InputKind},
		SetID: embedding.ID, VectorSpaceID: embedding.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	source, err := s.CaptureVectorIndexSource(t.Context(), embedding.VectorSpace.ID)
	require.NoError(t, err)
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	space := source.VectorSpaceID
	first := VectorIndexGenerationRecord{ID: hashVectorIndexTest("generation-a"),
		VectorSpaceID: space, SourceManifestChecksum: source.ManifestChecksum,
		IndexManifestChecksum: hashVectorIndexTest("manifest-a"), Bytes: []byte("generation-a"),
		RowCount: 1, BuiltAt: metadataEmbeddingTimeForTest(now)}
	first.ID = VectorIndexGenerationID(first.SourceManifestChecksum, first.Bytes)
	second := first
	second.Bytes = []byte("generation-b")
	second.ID = VectorIndexGenerationID(second.SourceManifestChecksum, second.Bytes)
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
	record.ID = VectorIndexGenerationID(record.SourceManifestChecksum, record.Bytes)
	require.NoError(t, putActiveVectorIndexGenerationForTest(t, s, record))
	now := time.Date(2026, 8, 26, 13, 0, 0, 0, time.UTC)
	_, claimed, err := s.ClaimVectorIndexBuild(t.Context(), record.VectorSpaceID,
		record.SourceManifestChecksum, "portable-metadata-build", now, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = s.AcquireVectorIndexGeneration(t.Context(), record.VectorSpaceID,
		"portable-metadata-reader", now, time.Minute)
	require.NoError(t, err)
	var seededJobs, seededLeases int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM vector_index_build_jobs),
		(SELECT COUNT(*) FROM vector_index_reader_leases)`).Scan(&seededJobs, &seededLeases))
	require.Equal(t, 1, seededJobs)
	require.Equal(t, 1, seededLeases)
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

	secondClaim, claimed, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID,
		source.ManifestChecksum, "replacement-worker", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	require.ErrorIs(t,
		s.StageVectorIndexGeneration(t.Context(), firstClaim, candidate, now.Add(2*time.Minute)),
		ErrVectorIndexBuildFenced)
	require.ErrorIs(t,
		s.PublishVectorIndexGeneration(t.Context(), firstClaim, candidate.ID, now.Add(2*time.Minute)),
		ErrVectorIndexBuildFenced)
	retry := candidate
	retry.BuiltAt = metadataEmbeddingTimeForTest(now.Add(2 * time.Minute))
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), secondClaim, retry, now.Add(2*time.Minute)))
	stored, err := s.LoadVectorIndexGeneration(t.Context(), candidate.ID)
	require.NoError(t, err)
	assert.Equal(t, candidate.BuiltAt, stored.BuiltAt, "the first complete candidate remains immutable")
}

func TestVectorIndexReclamationProtectsCandidateUnderLiveBuildClaim(t *testing.T) {
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
	now := time.Date(2026, 8, 26, 16, 0, 0, 0, time.UTC)
	claim, claimed, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID,
		source.ManifestChecksum, "live-build", now, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	candidate := vectorIndexGenerationFixture("live-build", source, []byte("live-build-candidate"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), claim, candidate, now))

	reclaimed, err := s.ReclaimVectorIndexGenerations(t.Context(), now.Add(30*time.Second))
	require.NoError(t, err)
	require.Zero(t, reclaimed)
	require.True(t, vectorIndexGenerationExistsForTest(t, s, candidate.ID))
	require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), claim, candidate.ID,
		now.Add(30*time.Second)))
}

func TestVectorIndexRetirementPreservesLiveBuildAndUnrelatedSpace(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	primary := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), primary))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: primary.BindingID,
			InputKind: primary.InputKind},
		SetID: primary.ID, VectorSpaceID: primary.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	otherDescriptor := vectorIndexOtherDescriptor(t)
	otherProfile := vectorIndexProfileWithDescriptor(t, profile, otherDescriptor)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return ensureProcessingProfileTx(t.Context(), tx, otherProfile)
	}))
	var otherVersion string
	require.NoError(t, s.db.QueryRow(`SELECT cv.version_id FROM content_versions cv
		JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
		WHERE cv.version_id<>? AND n.trashed_at IS NULL ORDER BY cv.version_id LIMIT 1`,
		versionID).Scan(&otherVersion))
	other := embeddingSetFixtureForDescriptor(s, otherVersion, otherProfile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "", otherDescriptor)
	require.NotEqual(t, primary.VectorSpace.ID, other.VectorSpace.ID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), other))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1,
		Key: EmbeddingHeadKey{ContentVersionID: otherVersion, BindingID: other.BindingID,
			InputKind: other.InputKind},
		SetID: other.ID, VectorSpaceID: other.VectorSpace.ID,
		ProcessingProfileFingerprint: otherProfile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	now := time.Date(2026, 8, 26, 17, 0, 0, 0, time.UTC)
	primarySource, err := s.CaptureVectorIndexSource(t.Context(), primary.VectorSpace.ID)
	require.NoError(t, err)
	primaryGeneration := vectorIndexGenerationFixture("retired-primary", primarySource,
		[]byte("retired-primary-generation"), now)
	publishVectorIndexGenerationForTest(t, s, primarySource, primaryGeneration, "primary-builder", now)
	otherSource, err := s.CaptureVectorIndexSource(t.Context(), other.VectorSpace.ID)
	require.NoError(t, err)
	otherGeneration := vectorIndexGenerationFixture("unrelated", otherSource,
		[]byte("unrelated-generation"), now)
	publishVectorIndexGenerationForTest(t, s, otherSource, otherGeneration, "other-builder", now)

	buildClaim, claimed, err := s.ClaimVectorIndexBuild(t.Context(), primary.VectorSpace.ID,
		primarySource.ManifestChecksum, "live-retired-build", now, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	candidate := vectorIndexGenerationFixture("live-retired-candidate", primarySource,
		[]byte("live-retired-candidate"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), buildClaim, candidate, now))

	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{versionID}})
	require.NoError(t, err)
	_, err = s.ActiveVectorIndexGeneration(t.Context(), primary.VectorSpace.ID)
	require.ErrorIs(t, err, ErrNotFound)
	activeOther, err := s.ActiveVectorIndexGeneration(t.Context(), other.VectorSpace.ID)
	require.NoError(t, err)
	require.Equal(t, otherGeneration.ID, activeOther.ID)

	reclaimed, err := s.ReclaimVectorIndexGenerations(t.Context(), now.Add(30*time.Second))
	require.NoError(t, err)
	require.Zero(t, reclaimed)
	require.True(t, vectorIndexGenerationExistsForTest(t, s, primaryGeneration.ID))
	require.True(t, vectorIndexGenerationExistsForTest(t, s, candidate.ID))
	require.True(t, vectorIndexGenerationExistsForTest(t, s, otherGeneration.ID))

	reclaimed, err = s.ReclaimVectorIndexGenerations(t.Context(), now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 2, reclaimed)
	require.False(t, vectorIndexGenerationExistsForTest(t, s, primaryGeneration.ID))
	require.False(t, vectorIndexGenerationExistsForTest(t, s, candidate.ID))
	require.True(t, vectorIndexGenerationExistsForTest(t, s, otherGeneration.ID))
}

func TestVectorIndexLoadPreflightsStoredShapeBeforeReturningGenerationBytes(t *testing.T) {
	s := newTestStore(t)
	bounds := vectorindex.Options{MaxRows: 1, MaxDimension: 1, MaxBytes: 8}
	for _, testCase := range []struct {
		name     string
		data     []byte
		declared int
		rows     int
		want     string
	}{
		{name: "stored length", data: bytes.Repeat([]byte{'x'}, 9), declared: 9, rows: 1,
			want: "shape exceeds bounds"},
		{name: "declared bytes", data: []byte("tiny"), declared: 9, rows: 1,
			want: "shape exceeds bounds"},
		{name: "row count", data: []byte("tiny"), declared: 4, rows: 2,
			want: "shape exceeds bounds"},
		{name: "declared mismatch", data: []byte("tiny"), declared: 5, rows: 1,
			want: "byte size is corrupt"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			id := hashVectorIndexTest("bounded-load-" + testCase.name)
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), `PRAGMA ignore_check_constraints=ON`); err != nil {
					return err
				}
				_, err := tx.ExecContext(t.Context(), `INSERT INTO vector_index_generations(
					generation_id,vector_space_id,source_manifest_checksum,index_manifest_checksum,
					generation_bytes,byte_size,row_count,built_at) VALUES(?,?,?,?,?,?,?,?)`,
					id, hashVectorIndexTest("bounded-space"), hashVectorIndexTest("bounded-source"),
					hashVectorIndexTest("bounded-manifest"), testCase.data, testCase.declared,
					testCase.rows, embeddingCatalogTime)
				return err
			}))
			loaded, err := loadVectorIndexGenerationTxWithBounds(t.Context(), s.db, id, bounds)
			require.ErrorContains(t, err, testCase.want)
			require.Empty(t, loaded.Bytes)
		})
	}
}

func TestVectorIndexSourcePreflightRejectsOversizedMembershipAndPayloadBeforeCapture(t *testing.T) {
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

	for _, test := range []struct {
		name   string
		bounds vectorindex.Options
		want   string
	}{
		{name: "membership", bounds: vectorindex.Options{MaxRows: 0, MaxDimension: 16_384, MaxBytes: 512 << 20},
			want: "membership exceeds"},
		{name: "payload", bounds: vectorindex.Options{MaxRows: 1, MaxDimension: 16_384,
			MaxBytes: int64(len(record.VectorSet.Payload) - 1)}, want: "payloads exceed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				_, err := preflightVectorIndexSource(t.Context(), tx, record.VectorSpace.ID, test.bounds)
				return err
			})
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestVectorIndexSchemaCorruptionFailsClosedOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	driver := DefaultSQLiteDriver()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(`ALTER TABLE vector_index_generations ADD COLUMN unexpected TEXT`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(path, driver)
	require.ErrorContains(t, err, "embedding catalog schema vector_index_generations")
}

func vectorIndexGenerationFixture(label string, source VectorIndexSource, data []byte, now time.Time) VectorIndexGenerationRecord {
	return VectorIndexGenerationRecord{ID: VectorIndexGenerationID(source.ManifestChecksum, data), VectorSpaceID: source.VectorSpaceID,
		SourceManifestChecksum: source.ManifestChecksum, IndexManifestChecksum: hashVectorIndexTest(label + "-manifest"),
		Bytes: data, RowCount: 1, BuiltAt: metadataEmbeddingTimeForTest(now)}
}

func publishVectorIndexGenerationForTest(t *testing.T, s *Store, source VectorIndexSource,
	record VectorIndexGenerationRecord, owner string, now time.Time,
) {
	t.Helper()
	claim, claimed, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID,
		source.ManifestChecksum, owner, now, time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), claim, record, now))
	require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), claim, record.ID, now))
}

func vectorIndexOtherDescriptor(t *testing.T) document.EmbeddingDescriptor {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-other-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "other document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "other query: {{content}}"},
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-other-embedding", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: hashVectorIndexTest("other-policy"), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-other-v1", ModelRevision: "2026-08-26", Dimension: 8,
		Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone,
		ScalarEncoding: "float32", DocumentFormatter: "other-document/v1", QueryFormatter: "other-query/v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile},
		CompatibilityID: "synthetic-other-space", SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	require.NoError(t, err)
	return descriptor
}

func vectorIndexProfileWithDescriptor(t *testing.T, base ProcessingProfileRecord,
	descriptor document.EmbeddingDescriptor,
) ProcessingProfileRecord {
	t.Helper()
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(base.CanonicalProfile, &profile))
	require.NotEmpty(t, profile.Embeddings)
	var binding document.EmbeddingBindingV1
	for _, candidate := range profile.Embeddings {
		if candidate.Name == "optional" {
			binding = candidate
			break
		}
	}
	require.Equal(t, "optional", binding.Name)
	binding.CompatibilityID = descriptor.CompatibilityID
	binding.Descriptor = document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}
	binding.Dimensions = descriptor.Dimension
	binding.DocumentFormatter = descriptor.DocumentFormatter
	binding.Metric = descriptor.Metric
	binding.Model = descriptor.Model
	binding.ModelInput = descriptor.ModelInput
	binding.Normalization = descriptor.Normalization
	binding.QueryFormatter = descriptor.QueryFormatter
	binding.ScalarEncoding = descriptor.ScalarEncoding
	binding.TrustBoundary = string(descriptor.TrustBoundary)
	profile.Embeddings = []document.EmbeddingBindingV1{binding}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: canonical,
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
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
