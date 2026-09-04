package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestEmbeddingCatalogRejectsRowsThatDivergeFromPayload(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	normalized, err := normalizeEmbeddingSetRecord(cloneEmbeddingSetRecord(record))
	require.NoError(t, err)
	record = normalized

	decoded, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
		MaxRows: 128, MaxDimension: record.VectorSpace.Dimensions, MaxBytes: len(record.VectorSet.Payload),
	})
	require.NoError(t, err)
	require.Len(t, decoded.Vectors, 2)
	values := make([][]float64, len(decoded.Vectors))
	for row := range decoded.Vectors {
		values[row] = make([]float64, len(decoded.Vectors[row]))
		for column, value := range decoded.Vectors[row] {
			values[row][column] = float64(value)
		}
	}
	decoded.InputKeys[0], decoded.InputKeys[1] = decoded.InputKeys[1], decoded.InputKeys[0]
	decoded.InputChecksums[0], decoded.InputChecksums[1] = decoded.InputChecksums[1], decoded.InputChecksums[0]
	values[0], values[1] = values[1], values[0]
	swapped, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: decoded.VectorSpaceFingerprint,
		Metric:                 decoded.Metric,
		Normalization:          decoded.Normalization,
		Dimension:              decoded.Dimension,
		InputKeys:              decoded.InputKeys,
		InputChecksums:         decoded.InputChecksums,
		Values:                 values,
	})
	require.NoError(t, err)
	payload, checksum, err := document.EncodeVectorSetV1(swapped)
	require.NoError(t, err)
	record.VectorSet.Payload = payload
	record.VectorSet.ID = checksum
	record.VectorSet.PayloadChecksum = checksum
	record.VectorSet.PayloadBlobHash = testSHA256(payload)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, record.VectorSet.PayloadBlobHash, int64(len(payload)))
	}))

	require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "input")
}

func TestEmbeddingCatalogHeadsAndFailuresAreProfileScoped(t *testing.T) {
	s, versionID, firstProfile, _ := newEmbeddingCatalogFixture(t)
	secondProfile := embeddingCatalogProfileVariant(t)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return ensureProcessingProfileTx(t.Context(), tx, secondProfile)
	}))

	first := embeddingSetFixture(s, versionID, firstProfile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	second := embeddingSetFixture(s, versionID, secondProfile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	second.ID = testSHA256([]byte("second-profile-embedding-set"))
	second.InputGeneration.ID = testSHA256([]byte("second-profile-input-generation"))
	require.NoError(t, s.StageEmbeddingSet(t.Context(), first))
	require.NoError(t, s.StageEmbeddingSet(t.Context(), second))
	for _, item := range []struct {
		profile ProcessingProfileRecord
		set     EmbeddingSetRecord
	}{
		{profile: firstProfile, set: first},
		{profile: secondProfile, set: second},
	} {
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			Key: EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional",
				InputKind: document.EmbeddingInputOriginalFile},
			SetID: item.set.ID, VectorSpaceID: item.set.VectorSpace.ID,
			ProcessingProfileFingerprint: item.profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
		require.NoError(t, s.RecordEmbeddingFailure(t.Context(), EmbeddingFailureRecord{
			ContentVersionID: versionID, ProcessingProfileFingerprint: item.profile.Fingerprint,
			BindingID: "required", InputKind: document.EmbeddingInputOriginalFile,
			FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: embeddingCatalogTime,
		}))
	}

	var heads, failures int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedding_heads WHERE content_version_id=?),
		(SELECT COUNT(*) FROM embedding_failures WHERE content_version_id=?)`,
		versionID, versionID).Scan(&heads, &failures))
	assert.Equal(t, 2, heads)
	assert.Equal(t, 2, failures)
}

func TestEmbeddingCatalogVocabularyIsValidatedOutsideSQLite(t *testing.T) {
	testCases := []struct {
		name, dropTrigger, before, mutation, want string
	}{
		{
			name: "vector-space contract", dropTrigger: "embedding_vector_spaces_immutable_update",
			mutation: `UPDATE embedding_vector_spaces SET contract_version='embedding-vector-space/v2'`,
			want:     "vector-space contract version is unsupported",
		},
		{
			name: "vector-set contract", dropTrigger: "embedding_vector_sets_immutable_update",
			mutation: `UPDATE embedding_vector_sets SET contract_version='vector-set/v2'`,
			want:     "vector-set contract version is unsupported",
		},
		{
			name: "set input kind", dropTrigger: "embedding_sets_immutable_update",
			before:   `DELETE FROM embedding_heads`,
			mutation: `UPDATE embedding_sets SET input_kind='future_input'`,
			want:     "embedding input kind",
		},
		{
			name:     "head input kind",
			mutation: `UPDATE embedding_heads SET input_kind='future_input'`,
			want:     "embedding input kind",
		},
		{
			name:     "failure input kind",
			mutation: `UPDATE embedding_failures SET input_kind='future_input'`,
			want:     "embedding input kind",
		},
		{
			name:     "failure code",
			mutation: `UPDATE embedding_failures SET failure_code='future_failure'`,
			want:     "provider-neutral vocabulary",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
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
			require.NoError(t, s.RecordEmbeddingFailure(t.Context(), EmbeddingFailureRecord{
				ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
				BindingID: "required", InputKind: document.EmbeddingInputOriginalFile,
				FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: embeddingCatalogTime,
			}))
			if testCase.dropTrigger != "" {
				_, err := s.db.Exec(`DROP TRIGGER ` + testCase.dropTrigger)
				require.NoError(t, err)
			}
			if testCase.before != "" {
				_, err := s.db.Exec(testCase.before)
				require.NoError(t, err)
			}
			_, err := s.db.Exec(testCase.mutation)
			require.NoError(t, err)
			require.ErrorContains(t, s.ValidateMetadata(t.Context()), testCase.want)
		})
	}
}

func TestEmbeddingCatalogMetadataRejectsFailureOutsideProfile(t *testing.T) {
	for _, testCase := range []struct {
		name, bindingID string
		inputKind       EmbeddingInputKind
		want            string
	}{
		{name: "missing binding", bindingID: "missing", inputKind: document.EmbeddingInputOriginalFile,
			want: "processing profile binding"},
		{name: "wrong input kind", bindingID: "chunk", inputKind: document.EmbeddingInputOriginalFile,
			want: "does not match processing profile binding kind"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			require.NoError(t, s.RecordEmbeddingFailure(t.Context(), EmbeddingFailureRecord{
				ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
				BindingID: "required", InputKind: document.EmbeddingInputOriginalFile,
				FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: embeddingCatalogTime,
			}))
			_, err := s.db.Exec(`UPDATE embedding_failures SET binding_id=?,input_kind=?`,
				testCase.bindingID, testCase.inputKind)
			require.NoError(t, err)
			require.ErrorContains(t, s.ValidateMetadata(t.Context()), testCase.want)
		})
	}
}

func TestEmbeddingCatalogPolicyLimitsAreValidatedOutsideSQLite(t *testing.T) {
	for _, testCase := range []struct {
		name, table, mutation, want string
		args                        []any
	}{
		{
			name: "descriptor bytes", table: "embedding_vector_spaces",
			mutation: `UPDATE embedding_vector_spaces SET descriptor_json=?`,
			args:     []any{strings.Repeat("x", (64<<10)+1)}, want: "descriptor JSON exceeds bounds",
		},
		{
			name: "projected text", table: "embedding_vector_spaces",
			mutation: `UPDATE embedding_vector_spaces SET provider_descriptor=?`,
			args:     []any{strings.Repeat("x", maxEmbeddingCatalogIDBytes+1)}, want: "provider descriptor",
		},
		{
			name: "dimensions", table: "embedding_vector_spaces",
			mutation: `UPDATE embedding_vector_spaces SET dimensions=?`,
			args:     []any{maxEmbeddingDimensions + 1}, want: "dimensions are invalid",
		},
		{
			name: "payload bytes", table: "embedding_vector_sets",
			mutation: `UPDATE embedding_vector_sets SET payload_size=?`,
			args:     []any{(64 << 20) + 1}, want: "payload size is invalid",
		},
		{
			name: "row count", table: "embedding_vector_sets",
			mutation: `UPDATE embedding_vector_sets SET row_count=?`,
			args:     []any{maxEmbeddingCatalogRows + 1}, want: "count or head authority is corrupt",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint,
				document.EmbeddingInputOriginalFile, "optional", "")
			require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
			_, err := s.db.Exec(`DROP TRIGGER embedding_` + strings.TrimPrefix(testCase.table, "embedding_") + `_immutable_update`)
			require.NoError(t, err)
			_, err = s.db.Exec(testCase.mutation, testCase.args...)
			require.NoError(t, err)
			require.ErrorContains(t, s.ValidateMetadata(t.Context()), testCase.want)
		})
	}
}

func TestEmbeddingCatalogVersionPruneRetainsPinnedArtifactsUntilGC(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	roots := []CurrentRenditionRoot{
		{ID: "pruned-generation-reader", Kind: RenditionRootReaderLease,
			TargetKind: RenditionRootEmbeddingGeneration, TargetID: record.InputGeneration.ID,
			FencingToken: 1, RecordedAt: embeddingCatalogTime, ExpiresAt: "2099-08-25T10:00:00.000000000Z"},
		{ID: "pruned-vector-backup", Kind: RenditionRootBackupPin,
			TargetKind: RenditionRootEmbeddingVectorSet, TargetID: record.VectorSet.ID,
			FencingToken: 1, RecordedAt: embeddingCatalogTime},
	}
	for _, root := range roots {
		require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), root))
	}

	var nodeID, revision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`,
		versionID).Scan(&nodeID, &revision))
	replacementHash := fakeHash("pinned-authority-prune-replacement")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return s.EnsureBlobTx(tx, replacementHash, 24)
	}))
	updated, _, err := s.ReplaceContent(t.Context(), nodeID, revision, replacementHash, 24, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), nodeID, updated.Revision,
		VersionPruneSelector{VersionIDs: []string{versionID}}, true)
	require.NoError(t, err)

	var versionAndSet, artifacts, activeRoots int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM content_versions WHERE version_id=?) +
		(SELECT COUNT(*) FROM embedding_sets WHERE embedding_set_id=?),
		(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?) +
		(SELECT COUNT(*) FROM embedding_vector_sets WHERE vector_set_id=?),
		(SELECT COUNT(*) FROM current_rendition_roots WHERE root_id IN (?,?) AND active=1)`,
		versionID, record.ID, record.InputGeneration.ID, record.VectorSet.ID,
		roots[0].ID, roots[1].ID).Scan(&versionAndSet, &artifacts, &activeRoots))
	assert.Zero(t, versionAndSet)
	assert.Equal(t, 2, artifacts)
	assert.Equal(t, 2, activeRoots)

	for _, root := range roots {
		released, releaseErr := s.ReleaseCurrentRenditionRoot(t.Context(), root.ID, root.FencingToken)
		require.NoError(t, releaseErr)
		require.True(t, released)
	}
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{})
	require.NoError(t, err)
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?) +
		(SELECT COUNT(*) FROM embedding_vector_sets WHERE vector_set_id=?)`,
		record.InputGeneration.ID, record.VectorSet.ID).Scan(&artifacts))
	assert.Zero(t, artifacts)
}

func TestEmbeddingCatalogSnapshotValidationChecksStoredRows(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint,
		document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `DELETE FROM embedding_sets WHERE embedding_set_id=?`, record.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `DROP TRIGGER embedding_vector_rows_immutable_update`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE embedding_vector_rows SET checksum=? WHERE vector_set_id=?`,
			fakeHash("corrupt-stored-vector-row"), record.VectorSet.ID)
		return err
	}))

	require.ErrorContains(t, s.ValidateMetadata(t.Context()), "manifest")
}

func embeddingCatalogProfileVariant(t *testing.T) ProcessingProfileRecord {
	t.Helper()
	base := embeddingCatalogProfile(t)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(base.CanonicalProfile, &profile))
	profile.Retrieval.VectorLimit++
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
