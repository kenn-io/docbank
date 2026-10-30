package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// Count actual catalog reads while all queries still execute against SQLite.
type descriptorReadQuerier struct {
	metadataQuerier

	reads int
}

func (q *descriptorReadQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "FROM embedding_vector_spaces WHERE") {
		q.reads++
	}
	return q.metadataQuerier.QueryRowContext(ctx, query, args...)
}

func newDescriptorReuseFixture(t *testing.T) (*Store, []EmbeddingSetRecord) {
	t.Helper()
	s, version, profile, attachment := newEmbeddingCatalogFixture(t)
	var second string
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? ORDER BY version_id LIMIT 1`, version).Scan(&second))
	records := []EmbeddingSetRecord{
		embeddingSetFixture(s, version, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", ""),
		embeddingSetFixture(s, second, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", ""),
	}
	for _, record := range records {
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: record.ContentVersionID, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	require.Equal(t, records[0].VectorSpace.ID, records[1].VectorSpace.ID)
	chunk := embeddingSetFixture(s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	require.Equal(t, records[0].VectorSpace.ID, chunk.VectorSpace.ID,
		"input kind changes input authority but these bindings intentionally share a vector space")
	// Inject a foreign catalog reference to pin malformed-member behavior.
	// It shares descriptor bytes but has a distinct actual stored space ID.
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

func TestEmbeddingSetDescriptorReadsBoundedByActualVectorSpaces(t *testing.T) {
	t.Parallel()
	s, records := newDescriptorReuseFixture(t)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		query := &descriptorReadQuerier{metadataQuerier: tx}
		spaces := make(map[string]EmbeddingVectorSpaceRecord)
		for _, index := range []int{0, 1, 2, 0, 2, 1} {
			want, err := loadEmbeddingSetTx(t.Context(), tx, records[index].ID)
			require.NoError(t, err)
			got, err := loadEmbeddingSetWithVectorSpacesTx(t.Context(), query, records[index].ID, spaces)
			require.NoError(t, err)
			require.Equal(t, want, got, "reuse must preserve all validated per-set authority")
		}
		require.Equal(t, 2, query.reads, "one strict descriptor read per actual vector space in a snapshot")
		return nil
	}))
}

func TestVectorIndexDescriptorReusePreservesManifestAndValidation(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"descriptor", "input count", "vector manifest", "wrong space", "wrong space corrupt descriptor"} {
		t.Run(mutation, func(t *testing.T) {
			s, records := newDescriptorReuseFixture(t)
			space := records[0].VectorSpace.ID
			want := make([]VectorIndexMember, 0, 2)
			for _, record := range records[:2] {
				want = append(want, VectorIndexMember{EmbeddingSetID: record.ID, VectorSetID: record.VectorSet.ID, PayloadBlobHash: record.VectorSet.PayloadBlobHash, PayloadSize: int64(len(record.VectorSet.Payload))})
			}
			slices.SortFunc(want, func(a, b VectorIndexMember) int { return strings.Compare(a.EmbeddingSetID, b.EmbeddingSetID) })
			before, err := s.CaptureVectorIndexSource(t.Context(), space)
			require.NoError(t, err)
			require.Equal(t, want, before.Members)
			require.Equal(t, vectorIndexSourceChecksum(want), before.ManifestChecksum)
			// Corruption injection deliberately bypasses the immutable-write guards.
			triggers := map[string][]string{
				"descriptor":                     {"embedding_vector_spaces_immutable_update"},
				"input count":                    {"embedding_input_generations_immutable_update"},
				"vector manifest":                {"embedding_vector_sets_immutable_update"},
				"wrong space":                    {"embedding_sets_immutable_update"},
				"wrong space corrupt descriptor": {"embedding_sets_immutable_update", "embedding_vector_spaces_immutable_update"},
			}
			for _, trigger := range triggers[mutation] {
				_, err := s.db.Exec("DROP TRIGGER " + trigger)
				require.NoError(t, err)
			}
			switch mutation {
			case "descriptor":
				_, err = s.db.Exec(`UPDATE embedding_vector_spaces SET descriptor_json=? WHERE vector_space_id=?`, []byte(`{}`), space)
			case "input count":
				_, err = s.db.Exec(`UPDATE embedding_input_generations SET input_count=input_count+1 WHERE generation_id=?`, records[0].InputGeneration.ID)
			case "vector manifest":
				_, err = s.db.Exec(`UPDATE embedding_vector_sets SET manifest_checksum=? WHERE vector_set_id=?`, testSHA256([]byte("corrupt-vector-manifest")), records[0].VectorSet.ID)
			case "wrong space", "wrong space corrupt descriptor":
				_, err = s.db.Exec(`UPDATE embedding_sets SET vector_space_id=? WHERE embedding_set_id=?`, records[2].VectorSpace.ID, records[0].ID)
				if err == nil && mutation == "wrong space corrupt descriptor" {
					_, err = s.db.Exec(`UPDATE embedding_vector_spaces SET descriptor_json=? WHERE vector_space_id=?`, []byte(`{}`), records[2].VectorSpace.ID)
				}
			}
			require.NoError(t, err)
			if mutation == "wrong space" {
				after, err := s.CaptureVectorIndexSource(t.Context(), space)
				require.NoError(t, err)
				expected := slices.DeleteFunc(want, func(member VectorIndexMember) bool { return member.EmbeddingSetID == records[0].ID })
				require.Equal(t, expected, after.Members)
				require.Equal(t, vectorIndexSourceChecksum(expected), after.ManifestChecksum)
				return
			}
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				_, originalErr := loadEmbeddingSetTx(t.Context(), tx, records[0].ID)
				require.Error(t, originalErr)
				_, err := captureVectorIndexSourceTx(t.Context(), tx, space)
				require.ErrorContains(t, err, originalErr.Error(), "a fresh capture must retain the original per-set corruption refusal, including mismatched members")
				return nil
			}))
		})
	}
}
