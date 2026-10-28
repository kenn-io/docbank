package store

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// Build the search fixtures used by the generation-reuse tests on the current
// catalog APIs. Keep one ordinary unembedded file in the seeded vault so the
// live-scope count covers both indexed and unindexed files.
func newSemanticPrefixFixture(t *testing.T, kind document.EmbeddingInputKind, count int) (*Store, ProcessingProfileRecord, []EmbeddingSetRecord, []vectorindex.Neighbor) {
	t.Helper()
	s, firstVersion, profile, firstAttachment := newEmbeddingCatalogFixture(t)
	binding := "optional"
	if kind == document.EmbeddingInputRenditionChunk {
		binding = "chunk"
	}
	var buildID string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, firstAttachment).Scan(&buildID))
	var records []EmbeddingSetRecord
	var neighbors []vectorindex.Neighbor
	for i := range count {
		versionID, attachmentID := firstVersion, ""
		if kind == document.EmbeddingInputRenditionChunk {
			attachmentID = firstAttachment
		}
		if i > 0 {
			node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("semantic-prefix-%03d.pdf", i), catalogSourceHash, 20, "application/pdf")
			require.NoError(t, err)
			versionID = node.CurrentVersionID
			if kind == document.EmbeddingInputRenditionChunk {
				attachmentID = testSHA256([]byte("semantic-prefix-attachment-" + versionID))
				require.NoError(t, publishAttachmentForTest(t, s, RenditionAttachmentRecord{
					ID: attachmentID, VaultID: s.VaultID(), ContentVersionID: versionID,
					BuildID: buildID, Profile: profile, AttachedAt: embeddingCatalogTime,
				}))
			}
		}
		record := embeddingSetFixture(t.Context(), s, versionID, profile.Fingerprint, kind, binding, attachmentID)
		if kind == document.EmbeddingInputRenditionChunk && i > 0 {
			vectors, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
				MaxRows: 128, MaxDimension: 8, MaxBytes: len(record.VectorSet.Payload),
			})
			require.NoError(t, err)
			for row := range vectors.Vectors {
				vectors.Vectors[row][0] += float32(i + 1)
			}
			payload, checksum, err := document.EncodeVectorSetV1(vectors)
			require.NoError(t, err)
			record.VectorSet.ID, record.VectorSet.PayloadChecksum = checksum, checksum
			record.VectorSet.Payload, record.VectorSet.PayloadBlobHash = payload, testSHA256(payload)
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				return s.EnsureBlobTx(t.Context(), tx, record.VectorSet.PayloadBlobHash, int64(len(payload)))
			}))
		}
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 1,
			Key:          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: binding, InputKind: kind},
			SetID:        record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
		records = append(records, record)
		for _, input := range record.InputGeneration.Inputs {
			neighbors = append(neighbors, vectorindex.Neighbor{
				SetID: record.VectorSet.ID, InputKey: input.ID,
				InputChecksum: input.RenderedChecksum, Score: 1 - float64(len(neighbors))/1000,
			})
		}
	}
	if kind == document.EmbeddingInputRenditionChunk && len(records) > 1 {
		require.NotEqual(t, records[0].VectorSet.ID, records[1].VectorSet.ID)
	}
	return s, profile, records, neighbors
}

func newDescriptorReuseFixture(t *testing.T) (*Store, []EmbeddingSetRecord) {
	t.Helper()
	s, firstVersion, profile, attachmentID := newEmbeddingCatalogFixture(t)
	var secondVersion string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT version_id FROM content_versions WHERE version_id<>? ORDER BY version_id LIMIT 1`, firstVersion).Scan(&secondVersion))
	records := []EmbeddingSetRecord{
		embeddingSetFixture(t.Context(), s, firstVersion, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", ""),
		embeddingSetFixture(t.Context(), s, secondVersion, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", ""),
	}
	for _, record := range records {
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 1,
			Key:          EmbeddingHeadKey{ContentVersionID: record.ContentVersionID, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID:        record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	require.Equal(t, records[0].VectorSpace.ID, records[1].VectorSpace.ID)
	chunk := embeddingSetFixture(t.Context(), s, firstVersion, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.Equal(t, records[0].VectorSpace.ID, chunk.VectorSpace.ID,
		"input kind changes input authority but these bindings intentionally share a vector space")
	foreign := records[0]
	foreign.ID = testSHA256([]byte("foreign-descriptor-set"))
	foreign.BindingID = "required"
	foreign.VectorSpace.ID = testSHA256([]byte("foreign-descriptor-space"))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		space, err := loadVectorSpaceTx(t.Context(), tx, records[0].VectorSpace.ID)
		if err != nil {
			return err
		}
		space.ID = foreign.VectorSpace.ID
		if err := insertVectorSpaceTx(t.Context(), tx, space); err != nil {
			return err
		}
		_, err = tx.ExecContext(t.Context(), `INSERT INTO embedding_sets(
			embedding_set_id,vault_uid,binding_id,input_kind,content_version_id,
			profile_fingerprint,embedding_input_fingerprint,vector_space_id,
			input_generation_id,vector_set_id,created_at)
			SELECT ?,vault_uid,?,input_kind,content_version_id,
			profile_fingerprint,embedding_input_fingerprint,?,
			input_generation_id,vector_set_id,created_at FROM embedding_sets WHERE embedding_set_id=?`,
			foreign.ID, foreign.BindingID, foreign.VectorSpace.ID, records[0].ID)
		return err
	}))
	return s, append(records, foreign)
}
