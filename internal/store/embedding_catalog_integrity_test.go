package store

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestEmbeddingCatalogExplicitPurgePreservesEveryActiveRoot(t *testing.T) {
	testCases := []struct {
		name       string
		kind       CurrentRenditionRootKind
		targetKind CurrentRenditionTargetKind
		target     func(EmbeddingSetRecord) string
		lease      bool
	}{
		{"reader lease set", RenditionRootReaderLease, RenditionRootEmbeddingSet, func(value EmbeddingSetRecord) string { return value.ID }, true},
		{"worker lease generation", RenditionRootWorkerLease, RenditionRootEmbeddingGeneration, func(value EmbeddingSetRecord) string { return value.InputGeneration.ID }, true},
		{"policy vector set", RenditionRootPolicyPin, RenditionRootEmbeddingVectorSet, func(value EmbeddingSetRecord) string { return value.VectorSet.ID }, false},
		{"restore payload", RenditionRootRestorePin, RenditionRootEmbeddingPayload, func(value EmbeddingSetRecord) string { return value.VectorSet.PayloadBlobHash }, false},
		{"backup set", RenditionRootBackupPin, RenditionRootEmbeddingSet, func(value EmbeddingSetRecord) string { return value.ID }, false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
			var nodeID, revision int64
			require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
			root := CurrentEmbeddingRoot{
				ID: "explicit-purge-" + strings.ReplaceAll(testCase.name, " ", "-"), Kind: testCase.kind,
				TargetKind: testCase.targetKind, TargetID: testCase.target(record), FencingToken: 1,
				RecordedAt: embeddingCatalogTime,
			}
			if testCase.lease {
				root.ExpiresAt = "2099-08-25T10:00:00.000000000Z"
			}
			require.NoError(t, s.PutCurrentEmbeddingRoot(t.Context(), root))

			report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{versionID}})
			require.NoError(t, err)
			assert.Zero(t, report.RemovedEmbeddingSets)
			assert.Zero(t, report.RemovedEmbeddingInputGenerations)
			assert.Zero(t, report.RemovedEmbeddingVectorSets)
			assert.True(t, report.ImmutableBackupCopiesUntouched)
			var authorities, activeRoots int
			require.NoError(t, s.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM embedding_sets WHERE embedding_set_id=?) +
				(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?) +
				(SELECT COUNT(*) FROM embedding_vector_sets WHERE vector_set_id=?) +
				(SELECT COUNT(*) FROM embedding_vector_spaces WHERE vector_space_id=?),
				(SELECT COUNT(*) FROM current_rendition_roots WHERE root_id=? AND active=1)`,
				record.ID, record.InputGeneration.ID, record.VectorSet.ID, record.VectorSpace.ID, root.ID,
			).Scan(&authorities, &activeRoots))
			assert.Equal(t, 4, authorities)
			assert.Equal(t, 1, activeRoots)
			require.NoError(t, s.ValidateMetadata(t.Context()))
			var exported bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &exported))

			_, _, err = s.Trash(t.Context(), nodeID, revision)
			require.NoError(t, err)
			plan, err := s.DerivativeGCPlan(t.Context())
			require.NoError(t, err)
			assert.Empty(t, plan.EmbeddingSets, "an active root must retain the complete selected set")

			// Releasing a reader, worker, policy, restore, or backup root is a separate
			// authority transition from live-vault purge.
			released, err := s.ReleaseCurrentEmbeddingRoot(t.Context(), root.ID, root.FencingToken)
			require.NoError(t, err)
			assert.True(t, released)
			plan, err = s.DerivativeGCPlan(t.Context())
			require.NoError(t, err)
			require.Len(t, plan.EmbeddingSets, 1)
			assert.Equal(t, record.ID, plan.EmbeddingSets[0].SetID)
			report, err = s.PurgeDerivatives(t.Context(), PurgeRequest{})
			require.NoError(t, err)
			assert.Equal(t, 1, report.RemovedEmbeddingSets)
			require.NoError(t, s.ValidateMetadata(t.Context()))
		})
	}
}

func TestEmbeddingCatalogExplicitChunkPurgeRetainsRootAttachmentChain(t *testing.T) {
	testCases := []struct {
		name          string
		kind          CurrentRenditionRootKind
		targetKind    CurrentRenditionTargetKind
		target        func(EmbeddingSetRecord) string
		request       func(string, string, string) PurgeRequest
		expires       bool
		removesDirect bool
	}{
		{
			name: "content version", kind: RenditionRootReaderLease,
			targetKind: RenditionRootEmbeddingSet, target: func(value EmbeddingSetRecord) string { return value.ID },
			request: func(versionID, _, _ string) PurgeRequest {
				return PurgeRequest{ContentVersionIDs: []string{versionID}}
			},
			expires: true, removesDirect: true,
		},
		{
			name: "attachment", kind: RenditionRootWorkerLease,
			targetKind: RenditionRootEmbeddingGeneration, target: func(value EmbeddingSetRecord) string { return value.InputGeneration.ID },
			request: func(_, attachmentID, _ string) PurgeRequest {
				return PurgeRequest{AttachmentIDs: []string{attachmentID}}
			},
			expires: true,
		},
		{
			name: "build", kind: RenditionRootPolicyPin,
			targetKind: RenditionRootEmbeddingVectorSet, target: func(value EmbeddingSetRecord) string { return value.VectorSet.ID },
			request: func(_, _, buildID string) PurgeRequest {
				return PurgeRequest{BuildIDs: []string{buildID}}
			},
		},
		{
			name: "all", kind: RenditionRootBackupPin,
			targetKind: RenditionRootEmbeddingPayload, target: func(value EmbeddingSetRecord) string { return value.VectorSet.PayloadBlobHash },
			request:       func(_, _, _ string) PurgeRequest { return PurgeRequest{All: true} },
			removesDirect: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
			var buildID string
			require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, attachmentID).Scan(&buildID))

			chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
			direct := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			for _, record := range []EmbeddingSetRecord{chunk, direct} {
				require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
				require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
					Key: EmbeddingHeadKey{
						ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind,
					},
					SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
					ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
				}))
			}
			root := CurrentEmbeddingRoot{
				ID: "rooted-chunk-" + strings.ReplaceAll(testCase.name, " ", "-"), Kind: testCase.kind,
				TargetKind: testCase.targetKind, TargetID: testCase.target(chunk), FencingToken: 1,
				RecordedAt: embeddingCatalogTime,
			}
			if testCase.expires {
				root.ExpiresAt = "2099-08-25T10:00:00.000000000Z"
			}
			require.NoError(t, s.PutCurrentEmbeddingRoot(t.Context(), root))
			request := testCase.request(versionID, attachmentID, buildID)

			report, err := s.PurgeDerivatives(t.Context(), request)
			require.NoError(t, err, "a rooted chunk generation must not roll back explicit purge")
			assert.Equal(t, 1, report.RemovedHeads)
			assert.Zero(t, report.RemovedAttachments)
			assert.Zero(t, report.RemovedBuilds)
			assert.Equal(t, []string{buildID}, report.RetainedBuildIDs)
			removedEmbeddingHeads := 1
			if testCase.removesDirect {
				removedEmbeddingHeads++
				assert.Equal(t, 1, report.RemovedEmbeddingSets,
					"root retention must not weaken explicit purge for an unrelated selected set")
			} else {
				assert.Zero(t, report.RemovedEmbeddingSets,
					"attachment/build selectors must not expand to a direct-file set")
			}
			assert.Equal(t, removedEmbeddingHeads, report.RemovedEmbeddingHeads)
			assert.True(t, report.ImmutableBackupCopiesUntouched)

			_, err = s.ActiveRendition(t.Context(), versionID, profile.Fingerprint)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = s.ActiveEmbeddingHead(t.Context(), EmbeddingHeadKey{
				ContentVersionID: versionID, BindingID: chunk.BindingID, InputKind: chunk.InputKind,
			})
			require.ErrorIs(t, err, ErrNotFound)
			var retained, directSets, directHeads int
			require.NoError(t, s.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM rendition_attachments WHERE attachment_id=?) +
				(SELECT COUNT(*) FROM rendition_builds WHERE build_id=?) +
				(SELECT COUNT(*) FROM embedding_sets WHERE embedding_set_id=?) +
				(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?) +
				(SELECT COUNT(*) FROM embedding_vector_sets WHERE vector_set_id=?) +
				(SELECT COUNT(*) FROM current_rendition_roots WHERE root_id=? AND active=1),
				(SELECT COUNT(*) FROM embedding_sets WHERE embedding_set_id=?),
				(SELECT COUNT(*) FROM embedding_heads WHERE embedding_set_id=?)`,
				attachmentID, buildID, chunk.ID, chunk.InputGeneration.ID, chunk.VectorSet.ID, root.ID,
				direct.ID, direct.ID,
			).Scan(&retained, &directSets, &directHeads))
			assert.Equal(t, 6, retained,
				"the rooted chunk set and its required attachment/build chain must remain complete")
			if testCase.removesDirect {
				assert.Equal(t, []int{0, 0}, []int{directSets, directHeads})
			} else {
				assert.Equal(t, []int{1, 1}, []int{directSets, directHeads})
			}
			require.NoError(t, s.ValidateMetadata(t.Context()))
			var exported bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &exported))

			if testCase.expires {
				root.FencingToken++
				root.RecordedAt = "2026-08-26T10:00:00.000000000Z"
				root.ExpiresAt = "2020-08-25T10:00:00.000000000Z"
				require.NoError(t, s.PutCurrentEmbeddingRoot(t.Context(), root))
			} else {
				released, err := s.ReleaseCurrentEmbeddingRoot(t.Context(), root.ID, root.FencingToken)
				require.NoError(t, err)
				assert.True(t, released)
			}
			plan, err := s.DerivativeGCPlan(t.Context())
			require.NoError(t, err)
			require.Len(t, plan.EmbeddingSets, 1)
			assert.Equal(t, chunk.ID, plan.EmbeddingSets[0].SetID)
			if testCase.expires {
				assert.Contains(t, plan.ExpiredRootIDs, root.ID)
			}

			report, err = s.PurgeDerivatives(t.Context(), request)
			require.NoError(t, err)
			assert.Equal(t, 1, report.RemovedAttachments)
			assert.Equal(t, 1, report.RemovedBuilds)
			assert.Equal(t, 1, report.RemovedEmbeddingSets)
			assert.Equal(t, 1, report.RemovedEmbeddingInputGenerations)
			assert.Equal(t, 1, report.RemovedEmbeddingVectorSets)
			if testCase.expires {
				assert.Equal(t, 1, report.ExpiredRootsRemoved)
			}
			var remaining int
			require.NoError(t, s.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM rendition_attachments WHERE attachment_id=?) +
				(SELECT COUNT(*) FROM rendition_builds WHERE build_id=?) +
				(SELECT COUNT(*) FROM embedding_sets WHERE embedding_set_id=?) +
				(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?) +
				(SELECT COUNT(*) FROM embedding_vector_sets WHERE vector_set_id=?)`,
				attachmentID, buildID, chunk.ID, chunk.InputGeneration.ID, chunk.VectorSet.ID,
			).Scan(&remaining))
			assert.Zero(t, remaining)
			require.NoError(t, s.ValidateMetadata(t.Context()))
			exported.Reset()
			require.NoError(t, s.ExportMetadata(t.Context(), &exported))
		})
	}
}

func TestEmbeddingCatalogCurrentSchemaMissingTableFailsClosedBeforeBootstrap(t *testing.T) {
	tables := []string{
		"embedding_vector_spaces", "embedding_input_generations", "embedding_generation_inputs",
		"embedding_vector_sets", "embedding_vector_rows", "embedding_sets", "embedding_heads",
		"embedding_failures", "embedding_corpus_generations", "embedding_corpus_members",
	}
	for _, driverCase := range v090UpgradeDrivers() {
		for _, table := range tables {
			t.Run(driverCase.name+"/"+table, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "catalog.db")
				s, err := Open(path, driverCase.driver)
				require.NoError(t, err)
				require.NoError(t, s.Close())
				db, err := driverCase.driver.Open(path, docsqlite.OpenOptions{
					Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
				})
				require.NoError(t, err)
				_, err = db.Exec(`DROP TABLE ` + table)
				require.NoError(t, err)
				require.NoError(t, db.Close())

				reopened, openErr := Open(path, driverCase.driver)
				if reopened != nil {
					require.NoError(t, reopened.Close())
				}
				require.ErrorContains(t, openErr, "embedding catalog schema")

				db, err = driverCase.driver.Open(path, docsqlite.OpenOptions{
					Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
				})
				require.NoError(t, err)
				columns, err := tableColumns(db, table)
				require.NoError(t, err)
				assert.Empty(t, columns, "failed open must not recreate missing current-schema authority")
				require.NoError(t, db.Close())
			})
		}
	}
}

func TestEmbeddingCatalogRestoreRejectsNoncanonicalDescriptorBytes(t *testing.T) {
	source, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(source, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, source.StageEmbeddingSet(t.Context(), record))
	var metadata bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &metadata))

	const privateMaterial = "synthetic-private-token"
	canonicalPrefix := fmt.Sprintf(`{"id":%q,"contract_version":1,`, record.VectorSpace.Descriptor.ID)
	reorderedPrefix := fmt.Sprintf(`{"contract_version":1,"id":%q,`, record.VectorSpace.Descriptor.ID)
	for _, testCase := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"unknown private field", func(value []byte) []byte {
			return bytes.Replace(value, []byte(`{"id":`), []byte(`{"private_material":"`+privateMaterial+`","id":`), 1)
		}},
		{"noncanonical field order", func(value []byte) []byte {
			return bytes.Replace(value, []byte(canonicalPrefix), []byte(reorderedPrefix), 1)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := mutateFirstProcessingMetadataRecord(
				t, metadata.Bytes(), metadataEmbeddingVectorSpaceType,
				func(fields map[string]jsontext.Value) {
					var descriptor []byte
					require.NoError(t, json.Unmarshal(fields["descriptor_json"], &descriptor))
					changed := testCase.mutate(descriptor)
					require.NotEqual(t, descriptor, changed)
					var err error
					fields["descriptor_json"], err = json.Marshal(changed)
					require.NoError(t, err)
				},
			)
			target := newTestStore(t)
			err := target.ImportMetadataForRestore(t.Context(), bytes.NewReader(mutated))
			require.ErrorContains(t, err, "descriptor")
			assert.NotContains(t, err.Error(), privateMaterial)
			var spaces int
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM embedding_vector_spaces`).Scan(&spaces))
			assert.Zero(t, spaces)
		})
	}
}

func TestEmbeddingCatalogRestoreRejectsCorpusMemberBindingAndVectorSpaceMismatch(t *testing.T) {
	source, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	member := embeddingSetFixture(source, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	otherBinding := embeddingSetFixture(source, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "required", "")
	require.NoError(t, source.StageEmbeddingSet(t.Context(), member))
	require.NoError(t, source.StageEmbeddingSet(t.Context(), otherBinding))
	require.NoError(t, source.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: member.BindingID, InputKind: member.InputKind},
		SetID: member.ID, VectorSpaceID: member.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	corpus := EmbeddingCorpusGenerationRecord{
		ID: testSHA256([]byte("final-review-corpus")), ContractVersion: EmbeddingCorpusGenerationV1,
		BindingID: member.BindingID, VectorSpaceID: member.VectorSpace.ID, SetIDs: []string{member.ID},
		ManifestChecksum: embeddingCorpusManifestChecksumForTest([]string{member.ID}), CreatedAt: embeddingCatalogTime,
	}
	require.NoError(t, source.StageEmbeddingCorpusGeneration(t.Context(), corpus))
	var metadata bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &metadata))

	t.Run("mixed binding", func(t *testing.T) {
		mutated := mutateFirstProcessingMetadataRecord(
			t, metadata.Bytes(), metadataEmbeddingCorpusType,
			func(fields map[string]jsontext.Value) {
				fields["manifest_checksum"] = jsonStringForTest(t, embeddingCorpusManifestChecksumForTest([]string{otherBinding.ID}))
			},
		)
		mutated = mutateFirstProcessingMetadataRecord(
			t, mutated, metadataEmbeddingCorpusMemberType,
			func(fields map[string]jsontext.Value) {
				fields["embedding_set_id"] = jsonStringForTest(t, otherBinding.ID)
			},
		)
		target := newTestStore(t)
		err := target.ImportMetadataForRestore(t.Context(), bytes.NewReader(mutated))
		require.ErrorContains(t, err, "binding or vector space")
	})

	t.Run("wrong vector space", func(t *testing.T) {
		otherSpaceID := fakeHash("foreign-key-valid-other-vector-space")
		mutated := duplicateEmbeddingVectorSpaceMetadata(t, metadata.Bytes(), member.VectorSpace.ID, otherSpaceID)
		mutated = mutateFirstProcessingMetadataRecord(
			t, mutated, metadataEmbeddingCorpusType,
			func(fields map[string]jsontext.Value) {
				fields["vector_space_id"] = jsonStringForTest(t, otherSpaceID)
			},
		)
		target := newTestStore(t)
		err := target.ImportMetadataForRestore(t.Context(), bytes.NewReader(mutated))
		require.ErrorContains(t, err, "binding or vector space")
	})
}

func TestEmbeddingCatalogLiveWritesRequireCanonicalTimestamps(t *testing.T) {
	const secretLikeTimestamp = "token=synthetic-private-value"

	t.Run("set", func(t *testing.T) {
		s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
		record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
		record.CreatedAt = secretLikeTimestamp
		require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "embedding set created_at")
		record.CreatedAt = embeddingCatalogTime
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	})

	t.Run("generation", func(t *testing.T) {
		s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
		record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
		record.InputGeneration.CreatedAt = "2026-08-25T10:00:00Z"
		require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "input-generation created_at")
		record.InputGeneration.CreatedAt = embeddingCatalogTime
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	})

	t.Run("head", func(t *testing.T) {
		s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
		record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		head := EmbeddingHeadRecord{
			Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: "2026-08-25T10:00:00.000000000+00:00",
		}
		require.ErrorContains(t, s.PublishEmbeddingHead(t.Context(), head), "embedding head published_at")
		head.PublishedAt = embeddingCatalogTime
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), head))
	})

	t.Run("failure", func(t *testing.T) {
		s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
		failure := EmbeddingFailureRecord{
			ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
			BindingID: "required", InputKind: document.EmbeddingInputOriginalFile,
			FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: "not-a-time",
		}
		require.ErrorContains(t, s.RecordEmbeddingFailure(t.Context(), failure), "embedding failure failed_at")
		failure.FailedAt = embeddingCatalogTime
		require.NoError(t, s.RecordEmbeddingFailure(t.Context(), failure))
	})

	t.Run("corpus", func(t *testing.T) {
		s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
		record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
		corpus := EmbeddingCorpusGenerationRecord{
			ID: testSHA256([]byte("timestamp-corpus")), ContractVersion: EmbeddingCorpusGenerationV1,
			BindingID: record.BindingID, VectorSpaceID: record.VectorSpace.ID, SetIDs: []string{record.ID},
			ManifestChecksum: embeddingCorpusManifestChecksumForTest([]string{record.ID}), CreatedAt: "tomorrow",
		}
		require.ErrorContains(t, s.StageEmbeddingCorpusGeneration(t.Context(), corpus), "embedding corpus created_at")
		corpus.CreatedAt = embeddingCatalogTime
		require.NoError(t, s.StageEmbeddingCorpusGeneration(t.Context(), corpus))
	})
}

func TestEmbeddingCatalogMetadataImportRequiresCanonicalTimestamps(t *testing.T) {
	source, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(source, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, source.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, source.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	require.NoError(t, source.RecordEmbeddingFailure(t.Context(), EmbeddingFailureRecord{
		ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
		BindingID: "required", InputKind: document.EmbeddingInputOriginalFile,
		FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: embeddingCatalogTime,
	}))
	require.NoError(t, source.StageEmbeddingCorpusGeneration(t.Context(), EmbeddingCorpusGenerationRecord{
		ID: testSHA256([]byte("metadata-timestamp-corpus")), ContractVersion: EmbeddingCorpusGenerationV1,
		BindingID: record.BindingID, VectorSpaceID: record.VectorSpace.ID, SetIDs: []string{record.ID},
		ManifestChecksum: embeddingCorpusManifestChecksumForTest([]string{record.ID}), CreatedAt: embeddingCatalogTime,
	}))
	var metadata bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &metadata))

	for _, testCase := range []struct {
		name, kind, field, value, want string
	}{
		{"generation", metadataEmbeddingGenerationType, "created_at", "token=synthetic-private-value", "input-generation created_at"},
		{"set", metadataEmbeddingSetType, "created_at", "not-a-time", "embedding set created_at"},
		{"head", metadataEmbeddingHeadType, "published_at", "2026-08-25T10:00:00Z", "embedding head published_at"},
		{"failure", metadataEmbeddingFailureType, "failed_at", "2026-08-25T10:00:00.000000000+00:00", "embedding failure failed_at"},
		{"corpus", metadataEmbeddingCorpusType, "created_at", "tomorrow", "embedding corpus created_at"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := mutateFirstProcessingMetadataRecord(
				t, metadata.Bytes(), testCase.kind,
				func(fields map[string]jsontext.Value) {
					fields[testCase.field] = jsonStringForTest(t, testCase.value)
				},
			)
			target := newTestStore(t)
			err := target.ImportMetadataForRestore(t.Context(), bytes.NewReader(mutated))
			require.ErrorContains(t, err, testCase.want)
			var rows int
			require.NoError(t, target.db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM embedding_sets) +
				(SELECT COUNT(*) FROM embedding_heads) +
				(SELECT COUNT(*) FROM embedding_failures) +
				(SELECT COUNT(*) FROM embedding_corpus_generations)`).Scan(&rows))
			assert.Zero(t, rows, "malformed timestamp import must roll back atomically")
		})
	}
}

func jsonStringForTest(t *testing.T, value string) jsontext.Value {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

func duplicateEmbeddingVectorSpaceMetadata(
	t *testing.T, input []byte, oldID, newID string,
) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(input), []byte{'\n'})
	for index, line := range lines {
		var kind struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &kind))
		if kind.Type != metadataEmbeddingVectorSpaceType {
			continue
		}
		duplicate := bytes.Replace(
			line,
			[]byte(`"vector_space_id":"`+oldID+`"`),
			[]byte(`"vector_space_id":"`+newID+`"`),
			1,
		)
		require.NotEqual(t, line, duplicate)
		lines = append(lines, nil)
		copy(lines[index+2:], lines[index+1:])
		lines[index+1] = duplicate
		return append(bytes.Join(lines, []byte{'\n'}), '\n')
	}
	require.FailNow(t, "embedding vector-space metadata record not found")
	return nil
}
