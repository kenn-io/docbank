package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func compareSingletonCaptureTx(t *testing.T, s *Store, tx *sql.Tx, record EmbeddingSetRecord, opts SearchOptions) {
	t.Helper()
	legacyCollector := newOriginalSemanticSnapshot(record.ProcessingProfileFingerprint, record.BindingID, record.VectorSpace.ID, record.InputKind, opts, true)
	collector := newOriginalSemanticSnapshot(record.ProcessingProfileFingerprint, record.BindingID, record.VectorSpace.ID, record.InputKind, opts, true)
	var observe vectorIndexSourceObserver
	if legacyCollector != nil {
		observe = legacyCollector.observe
	}
	want, wantErr := captureVectorIndexSourceObservedTx(t.Context(), tx, record.VectorSpace.ID, observe)
	got, gotErr := s.captureSemanticSearchSourceTx(t.Context(), tx, record.VectorSpace.ID, collector)
	originalSnapshotErrorParity(t, wantErr, gotErr)
	require.Equal(t, want, got, "exact ordered source and manifest parity")
	if wantErr == nil {
		require.Equal(t, legacyCollector, collector, "typed singleton and unchanged full-record collector agree")
	}
	// The metadata-only lease uses the same capture without a collector.
	want, wantErr = captureVectorIndexSourceTx(t.Context(), tx, record.VectorSpace.ID)
	got, gotErr = s.captureSemanticSearchSourceTx(t.Context(), tx, record.VectorSpace.ID, nil)
	originalSnapshotErrorParity(t, wantErr, gotErr)
	require.Equal(t, want, got)
}

func TestSemanticSearchSourceSingletonFallbackParity(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"historical", "directory", "attachment", "missing input", "extra input", "missing vector row", "extra vector row", "extra input and vector", "input ordinal", "vector ordinal", "input numeric metadata", "vector numeric metadata", "missing set", "missing generation", "missing vector set", "historical corrupt count", "trashed corrupt manifest"} {
		t.Run(mutation, func(t *testing.T) {
			s, _, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 3, false)
			record := records[0]
			rolledBack := errors.New("rollback synthetic corruption")
			err := s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`PRAGMA defer_foreign_keys=ON`)
				require.NoError(t, err)
				_, err = tx.Exec(`PRAGMA ignore_check_constraints=ON`)
				require.NoError(t, err)
				defer func() { _, err := tx.Exec(`PRAGMA ignore_check_constraints=OFF`); require.NoError(t, err) }()
				for _, name := range []string{"embedding_sets", "embedding_input_generations", "embedding_vector_sets", "embedding_generation_inputs", "embedding_vector_rows"} {
					_, err := tx.Exec(`DROP TRIGGER ` + name + `_immutable_update`)
					require.NoError(t, err)
				}
				switch mutation {
				case "historical", "historical corrupt count":
					_, err = tx.Exec(`UPDATE nodes SET current_version_id=NULL WHERE current_version_id=?`, record.ContentVersionID)
					require.NoError(t, err)
					if mutation == "historical corrupt count" {
						_, err = tx.Exec(`UPDATE embedding_input_generations SET input_count=2 WHERE generation_id=?`, record.InputGeneration.ID)
					}
				case "directory":
					_, err = tx.Exec(`UPDATE nodes SET kind='dir' WHERE current_version_id=?`, record.ContentVersionID)
				case "attachment":
					_, err = tx.Exec(`UPDATE embedding_input_generations SET attachment_id=(SELECT attachment_id FROM rendition_attachments LIMIT 1) WHERE generation_id=?`, record.InputGeneration.ID)
				case "missing input":
					_, err = tx.Exec(`DELETE FROM embedding_generation_inputs WHERE generation_id=?`, record.InputGeneration.ID)
				case "extra input":
					_, err = tx.Exec(`INSERT INTO embedding_generation_inputs SELECT generation_id,'extra-input',input_order+1,rendered_checksum FROM embedding_generation_inputs WHERE generation_id=?`, record.InputGeneration.ID)
				case "missing vector row":
					_, err = tx.Exec(`DELETE FROM embedding_vector_rows WHERE vector_set_id=?`, record.VectorSet.ID)
				case "extra vector row":
					_, err = tx.Exec(`INSERT INTO embedding_vector_rows SELECT vector_set_id,'extra-row',row_order+1,'extra-input',dimensions,checksum FROM embedding_vector_rows WHERE vector_set_id=?`, record.VectorSet.ID)
				case "extra input and vector":
					_, err = tx.Exec(`INSERT INTO embedding_generation_inputs SELECT generation_id,'extra-input',input_order+1,rendered_checksum FROM embedding_generation_inputs WHERE generation_id=?`, record.InputGeneration.ID)
					require.NoError(t, err)
					_, err = tx.Exec(`INSERT INTO embedding_vector_rows SELECT vector_set_id,'extra-row',row_order+1,'extra-input',dimensions,checksum FROM embedding_vector_rows WHERE vector_set_id=?`, record.VectorSet.ID)
				case "input ordinal":
					_, err = tx.Exec(`UPDATE embedding_generation_inputs SET input_order=7 WHERE generation_id=?`, record.InputGeneration.ID)
				case "vector ordinal":
					_, err = tx.Exec(`UPDATE embedding_vector_rows SET row_order=7 WHERE vector_set_id=?`, record.VectorSet.ID)
				case "input numeric metadata":
					_, err = tx.Exec(`UPDATE embedding_input_generations SET generation_encoded_size='not-an-integer' WHERE generation_id=?`, record.InputGeneration.ID)
				case "vector numeric metadata":
					_, err = tx.Exec(`UPDATE embedding_vector_sets SET payload_size=1.5 WHERE vector_set_id=?`, record.VectorSet.ID)
				case "missing set":
					_, err = tx.Exec(`DELETE FROM embedding_sets WHERE embedding_set_id=?`, record.ID)
				case "missing generation":
					_, err = tx.Exec(`DELETE FROM embedding_input_generations WHERE generation_id=?`, record.InputGeneration.ID)
				case "missing vector set":
					_, err = tx.Exec(`DELETE FROM embedding_vector_sets WHERE vector_set_id=?`, record.VectorSet.ID)
				case "trashed corrupt manifest":
					_, err = tx.Exec(`UPDATE nodes SET trashed_at=? WHERE current_version_id=?`, embeddingCatalogTime, record.ContentVersionID)
					require.NoError(t, err)
					_, err = tx.Exec(`UPDATE embedding_vector_sets SET manifest_checksum=? WHERE vector_set_id=?`, strings.Repeat("a", 64), record.VectorSet.ID)
				}
				require.NoError(t, err)
				compareSingletonCaptureTx(t, s, tx, record, SearchOptions{})
				compareSingletonCaptureTx(t, s, tx, record, SearchOptions{ContentVersionIDs: []string{records[1].ContentVersionID}})
				return rolledBack
			})
			require.ErrorIs(t, err, rolledBack)
		})
	}
}

func TestSemanticSearchSourceSingletonActualSpaceDescriptor(t *testing.T) {
	t.Parallel()
	for _, corrupt := range []bool{false, true} {
		s, records := newDescriptorReuseFixture(t)
		makeSingletonSourceHistorical(t, s, records[0])
		var retainedHeads int
		require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM embedding_heads WHERE embedding_set_id=?`, records[0].ID).Scan(&retainedHeads))
		require.Equal(t, 1, retainedHeads, "historical member remains a real source-validation candidate")
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`DROP TRIGGER embedding_sets_immutable_update`)
			require.NoError(t, err)
			_, err = tx.Exec(`UPDATE embedding_sets SET vector_space_id=? WHERE embedding_set_id=?`, records[2].VectorSpace.ID, records[0].ID)
			require.NoError(t, err)
			// The historical member must still validate its actual descriptor.
			if corrupt {
				_, err = tx.Exec(`DROP TRIGGER embedding_vector_spaces_immutable_update`)
				require.NoError(t, err)
				_, err = tx.Exec(`UPDATE embedding_vector_spaces SET descriptor_json=? WHERE vector_space_id=?`, []byte(`{}`), records[2].VectorSpace.ID)
				require.NoError(t, err)
				_, legacyErr := captureVectorIndexSourceTx(t.Context(), tx, records[0].VectorSpace.ID)
				require.ErrorContains(t, legacyErr, records[0].ID, "the historical member must refuse its actual corrupt descriptor")
			}
			compareSingletonCaptureTx(t, s, tx, records[0], SearchOptions{})
			return nil
		}))
	}
}

// Compose metadata/storage-class mutations rather than fuzzing an isolated
// checksum helper. Equality with the unchanged capture is the contract.
func FuzzSemanticSearchSourceSingletonParity(f *testing.F) {
	for _, seed := range [][]byte{{}, {0}, {1, 2, 3}, {4, 5, 6, 7}, {255, 128, 64}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, mutations []byte) {
		s, _, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 2, false)
		// Materialization is bounded; byte values and their combinations stay broad.
		if len(mutations) > 8 {
			mutations = mutations[:8]
		}
		// Normal writes run before the corruption transaction; every historical
		// target retains a current version of the same file, never a NULL head.
		for i, mutation := range mutations {
			if mutation%8 == 6 {
				makeSingletonSourceHistorical(t, s, records[i%len(records)])
			}
		}
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			for _, name := range []string{"embedding_input_generations", "embedding_generation_inputs", "embedding_vector_sets", "embedding_vector_rows"} {
				_, err := tx.Exec(`DROP TRIGGER ` + name + `_immutable_update`)
				require.NoError(t, err)
			}
			for i, mutation := range mutations {
				record := records[i%len(records)]
				var err error
				switch mutation % 8 {
				case 0:
					_, err = tx.Exec(`UPDATE nodes SET trashed_at=? WHERE current_version_id=?`, embeddingCatalogTime, record.ContentVersionID)
				case 1:
					_, err = tx.Exec(`UPDATE embedding_generation_inputs SET rendered_checksum=? WHERE generation_id=?`, testSHA256([]byte{mutation}), record.InputGeneration.ID)
				case 2:
					_, err = tx.Exec(`UPDATE embedding_generation_inputs SET rendered_checksum=CAST(rendered_checksum AS BLOB) WHERE generation_id=?`, record.InputGeneration.ID)
				case 3:
					_, err = tx.Exec(`UPDATE embedding_vector_rows SET checksum=CAST(checksum AS BLOB) WHERE vector_set_id=?`, record.VectorSet.ID)
				case 4:
					_, err = tx.Exec(`UPDATE embedding_input_generations SET input_count=input_count+1 WHERE generation_id=?`, record.InputGeneration.ID)
				case 5:
					_, err = tx.Exec(`UPDATE embedding_vector_sets SET manifest_checksum=? WHERE vector_set_id=?`, testSHA256([]byte{mutation}), record.VectorSet.ID)
				case 6:
					continue // Valid historical replacement was prepared above.
				case 7:
					_, err = tx.Exec(`UPDATE embedding_generation_inputs SET input_id=CAST(input_id AS BLOB) WHERE generation_id=?`, record.InputGeneration.ID)
				}
				require.NoError(t, err)
			}
			compareSingletonCaptureTx(t, s, tx, records[0], SearchOptions{})
			return nil
		}))
	})
}

func makeSingletonSourceHistorical(t *testing.T, s *Store, record EmbeddingSetRecord) {
	t.Helper()
	version, err := s.ContentVersionByID(t.Context(), record.ContentVersionID)
	require.NoError(t, err)
	node, err := s.NodeByID(t.Context(), version.NodeID)
	require.NoError(t, err)
	// Replacing identical bytes still creates an exact new immutable version.
	_, replacement, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, node.BlobHash, node.Size, node.MimeType)
	require.NoError(t, err)
	require.NotEqual(t, record.ContentVersionID, replacement.ID)
}
