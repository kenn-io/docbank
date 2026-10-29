//go:build cgo

package store

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestOriginalSemanticSnapshotLexicalErrorPriority(t *testing.T) {
	t.Parallel()
	s, profile, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 2, false)
	source, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
	require.NoError(t, err)
	requested := originalSnapshotEvidence(t, s, records[0], source.ManifestChecksum)
	var build, segment string
	require.NoError(t, s.db.QueryRow(`SELECT build_id,segment_id FROM rendition_lexical_segments ORDER BY build_id,segment_id LIMIT 1`).Scan(&build, &segment))
	requested[0].Evidence = append(requested[0].Evidence, SearchEvidenceIdentity{Kind: "rendition_segment", BuildID: build, SegmentID: segment})
	_, err = s.db.Exec(`DROP TRIGGER embedding_vector_spaces_immutable_update`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE embedding_vector_spaces SET descriptor_json=? WHERE vector_space_id=?`, []byte(`{}`), records[0].VectorSpace.ID)
	require.NoError(t, err)
	var schemaReads atomic.Int64
	install := func(ctx context.Context, deny bool) {
		conn, err := s.writeDB.Conn(ctx)
		require.NoError(t, err)
		defer func() { require.NoError(t, conn.Close()) }() // Always release before caller revalidation, including require.Goexit.
		require.NoError(t, conn.Raw(func(raw any) error {
			actual, ok := raw.(*sqlite3.SQLiteConn)
			require.True(t, ok, "CGO default must expose actual mattn connection")
			if !deny {
				actual.RegisterAuthorizer(nil)
				return nil
			}
			actual.RegisterAuthorizer(func(action int, a, b, c string) int {
				if action == sqlite3.SQLITE_READ && (a == "sqlite_schema" || a == "sqlite_master") {
					schemaReads.Add(1)
					return sqlite3.SQLITE_DENY
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		}))
	}
	install(t.Context(), true)
	defer install(context.Background(), false)
	// Obtain the actual owning schema-read failure independently of source capture.
	var oldErr error
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var projection bool
		oldErr = tx.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE type='table' AND name='rendition_lexical_heads')`).Scan(&projection)
		return nil
	}))
	require.Error(t, oldErr)
	_, newErr := s.RevalidateSearchCandidates(t.Context(), requested, SearchOptions{}, profile.Fingerprint, records[0].BindingID)
	originalSnapshotErrorParity(t, oldErr, newErr)
	require.Error(t, newErr)
	require.GreaterOrEqual(t, schemaReads.Load(), int64(2), "both actual lexical schema probes reached")
	t.Log("LEXICAL_SCHEMA_ERROR_WINS_OVER_SAVED_SOURCE")
}

// Cancel after the actual lease INSERT has been prepared and committed, at the
// next expanded head SELECT. The callback is installed on the sole writeDB
// connection and released before acquisition; there is no production test hook.
func TestOriginalSemanticSnapshotAcquisitionCancellationReleasesLease(t *testing.T) {
	t.Parallel()
	s, p, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 2, false)
	source, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
	require.NoError(t, err)
	var sets []document.VectorSetV1
	var ids []string
	for _, record := range records {
		vectors, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{MaxRows: 128, MaxDimension: 8, MaxBytes: len(record.VectorSet.Payload)})
		require.NoError(t, err)
		sets = append(sets, vectors)
		ids = append(ids, record.VectorSet.ID)
	}
	manifest, err := vectorindex.NewManifest(ids)
	require.NoError(t, err)
	generation, err := vectorindex.BuildGeneration(manifest, sets, vectorindex.Options{})
	require.NoError(t, err)
	stored := VectorIndexGenerationRecord{ID: hashVectorIndexTest("snapshot-cancellation-generation"), VectorSpaceID: source.VectorSpaceID, SourceManifestChecksum: source.ManifestChecksum, IndexManifestChecksum: generation.Metadata().Manifest.Checksum, Bytes: generation.Bytes(), RowCount: generation.Metadata().RowCount, BuiltAt: embeddingCatalogTime}
	require.NoError(t, putVectorIndexGenerationForTest(t, s, stored))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var inserted, cancelled atomic.Bool
	install := func(enabled bool) {
		conn, err := s.writeDB.Conn(context.Background())
		require.NoError(t, err)
		defer func() { require.NoError(t, conn.Close()) }()
		require.NoError(t, conn.Raw(func(raw any) error {
			actual, ok := raw.(*sqlite3.SQLiteConn)
			require.True(t, ok)
			if !enabled {
				actual.RegisterAuthorizer(nil)
				return nil
			}
			actual.RegisterAuthorizer(func(action int, a, b, c string) int {
				if action == sqlite3.SQLITE_INSERT && a == "vector_index_reader_leases" {
					inserted.Store(true)
				}
				if action == sqlite3.SQLITE_READ && a == "embedding_heads" && inserted.Load() && cancelled.CompareAndSwap(false, true) {
					cancel()
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		}))
	}
	install(true)
	defer install(false)
	authority, err := s.AcquireSemanticSearchAuthority(ctx, p.Fingerprint, records[0].BindingID, "snapshot-cancel", time.Now().UTC(), time.Minute, SearchOptions{})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, authority, "no partial counts/ANN or authority may escape")
	require.True(t, inserted.Load())
	require.True(t, cancelled.Load(), "expanded authority fence reached after real lease insertion")
	var leases int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_index_reader_leases`).Scan(&leases))
	require.Zero(t, leases, "WithoutCancel release removes the committed lease")
	t.Log("ACTUAL_ACQUISITION_CANCELLATION_NO_PARTIAL_AUTHORITY_AND_NO_READER_LEASE")
}
