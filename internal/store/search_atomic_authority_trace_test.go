//go:build cgo && sqlite_trace

package store

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// The public metadata-only acquisition must validate and lease one fresh source
// snapshot. TraceStmt observes actual executions, including cached statements.
func TestSemanticSearchAuthorityHeadAtomicSourceAcquisition(t *testing.T) {
	t.Parallel()
	s, profile, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 1, false)
	record := records[0]
	var expectedSpace EmbeddingVectorSpaceRecord
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var err error
		expectedSpace, err = loadVectorSpaceTx(t.Context(), tx, record.VectorSpace.ID)
		return err
	}))
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	require.Len(t, source.Members, 1)
	vectors, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{MaxRows: 128, MaxDimension: 8, MaxBytes: len(record.VectorSet.Payload)})
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{record.VectorSet.ID})
	require.NoError(t, err)
	generation, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{vectors}, vectorindex.Options{})
	require.NoError(t, err)
	stored := VectorIndexGenerationRecord{ID: hashVectorIndexTest("atomic-authority-generation"), VectorSpaceID: source.VectorSpaceID,
		SourceManifestChecksum: source.ManifestChecksum, IndexManifestChecksum: generation.Metadata().Manifest.Checksum,
		Bytes: generation.Bytes(), RowCount: generation.Metadata().RowCount, BuiltAt: embeddingCatalogTime}
	require.NoError(t, putVectorIndexGenerationForTest(t, s, stored))
	var captures, begins, commits atomic.Int64
	install := func(enabled bool) {
		conn, err := s.writeDB.Conn(context.Background())
		require.NoError(t, err)
		defer func() { require.NoError(t, conn.Close()) }()
		require.NoError(t, conn.Raw(func(raw any) error {
			actual, ok := raw.(*sqlite3.SQLiteConn)
			require.True(t, ok)
			if !enabled {
				return actual.SetTrace(nil)
			}
			return actual.SetTrace(&sqlite3.TraceConfig{EventMask: sqlite3.TraceStmt, Callback: func(info sqlite3.TraceInfo) int {
				statement := strings.TrimSpace(info.StmtOrTrigger)
				if statement == strings.TrimSpace(canonicalOriginalSingletonQuery) {
					captures.Add(1)
				}
				switch {
				case strings.HasPrefix(strings.ToUpper(statement), "BEGIN"):
					begins.Add(1)
				case strings.EqualFold(statement, "COMMIT"):
					commits.Add(1)
				}
				return 0
			}})
		}))
	}
	install(true)
	defer install(false)
	now := time.Now().UTC()
	authority, err := s.AcquireSemanticSearchAuthorityHead(t.Context(), profile.Fingerprint, record.BindingID, "atomic-reader", now, time.Minute, SearchOptions{})
	install(false)
	require.NoError(t, err)
	require.Equal(t, expectedSpace, authority.VectorSpace)
	require.Equal(t, document.EmbeddingInputOriginalFile, authority.InputKind)
	require.False(t, authority.BindingRequired)
	require.Equal(t, 2, authority.ScopedDocuments, "fixture retains one unembedded document")
	require.Equal(t, 1, authority.CompleteDocuments)
	require.Equal(t, []vectorindex.RowIdentity{{SetID: record.VectorSet.ID, InputKey: record.InputGeneration.Inputs[0].ID,
		InputChecksum: record.InputGeneration.Inputs[0].RenderedChecksum}}, authority.ANNRows)
	stored.ByteSize = len(stored.Bytes)
	stored.Bytes = nil
	require.Equal(t, stored, authority.Lease.Generation)
	require.NotEmpty(t, authority.Lease.ID)
	require.Equal(t, int64(1), authority.Lease.FencingToken)
	require.Equal(t, now.Add(time.Minute), authority.Lease.ExpiresAt)
	var leaseID, generationID, owner string
	var token int64
	require.NoError(t, s.db.QueryRow(`SELECT lease_id,generation_id,owner,fencing_token FROM vector_index_reader_leases`).Scan(&leaseID, &generationID, &owner, &token))
	require.Equal(t, authority.Lease.ID, leaseID)
	require.Equal(t, stored.ID, generationID)
	require.Equal(t, "atomic-reader", owner)
	require.Equal(t, authority.Lease.FencingToken, token)
	require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), leaseID, token, now))
	var leases int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM vector_index_reader_leases`).Scan(&leases))
	require.Zero(t, leases)
	// Functional and cleanup assertions precede the resource regression budget.
	t.Logf("initial source captures=%d BEGIN=%d COMMIT=%d", captures.Load(), begins.Load(), commits.Load())
	require.Equal(t, int64(1), captures.Load(), "initial lease and ANN authority share one complete fresh source capture")
	require.Equal(t, int64(1), begins.Load(), "initial authority and lease share one writer transaction")
	require.Equal(t, int64(1), commits.Load())
}
