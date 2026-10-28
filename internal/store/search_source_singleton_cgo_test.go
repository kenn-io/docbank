//go:build cgo && sqlite_trace

package store

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// Count actual SELECT executions, including cached statements, on the owning
// connection through the public metadata-only authority API and both fresh
// lease/source fences. Run explicitly with -tags "fts5 sqlite_trace".
func TestSemanticSearchSourceSingletonQueryGrowth(t *testing.T) {
	t.Parallel()
	counts := make([]int64, 0, 2)
	for _, size := range []int{1, 16} {
		s, p, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, size, false)
		source, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
		require.NoError(t, err)
		require.Len(t, source.Members, size)
		now := time.Now().UTC()
		generation := vectorIndexGenerationFixture("singleton-query-growth", source, []byte("synthetic-index"), now)
		generation.RowCount = size
		require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
		var selects, preparations atomic.Int64
		install := func(enabled bool) {
			conn, err := s.writeDB.Conn(context.Background())
			require.NoError(t, err)
			defer func() { require.NoError(t, conn.Close()) }()
			require.NoError(t, conn.Raw(func(raw any) error {
				actual, ok := raw.(*sqlite3.SQLiteConn)
				require.True(t, ok)
				if !enabled {
					actual.RegisterAuthorizer(nil)
					return actual.SetTrace(nil)
				}
				actual.RegisterAuthorizer(func(action int, a, b, c string) int {
					if action == sqlite3.SQLITE_SELECT {
						preparations.Add(1)
					}
					return sqlite3.SQLITE_OK
				})
				return actual.SetTrace(&sqlite3.TraceConfig{EventMask: sqlite3.TraceStmt, Callback: func(info sqlite3.TraceInfo) int {
					if info.EventCode == sqlite3.TraceStmt && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(info.StmtOrTrigger)), "SELECT") {
						selects.Add(1)
					}
					return 0
				}})
			}))
		}
		install(true)
		authority, err := s.AcquireSemanticSearchAuthorityHead(t.Context(), p.Fingerprint, records[0].BindingID, "query-growth", now, time.Minute, SearchOptions{})
		install(false)
		require.NoError(t, err)
		counts = append(counts, selects.Load())
		t.Logf("members=%d SELECT executions=%d preparations=%d", size, selects.Load(), preparations.Load())
		require.Equal(t, source.ManifestChecksum, authority.Lease.Generation.SourceManifestChecksum)
		require.Equal(t, generation.ID, authority.Lease.Generation.ID)
		require.Empty(t, authority.Lease.Generation.Bytes)
		require.Equal(t, size+1, authority.ScopedDocuments, "fixture also retains one unembedded document")
		require.Equal(t, size, authority.CompleteDocuments)
		var wantANN []vectorindex.RowIdentity
		for _, record := range records {
			wantANN = append(wantANN, vectorindex.RowIdentity{SetID: record.VectorSet.ID, InputKey: record.InputGeneration.Inputs[0].ID, InputChecksum: record.InputGeneration.Inputs[0].RenderedChecksum})
		}
		require.ElementsMatch(t, wantANN, authority.ANNRows)
		expectedGeneration := generation
		expectedGeneration.ByteSize = len(generation.Bytes)
		expectedGeneration.Bytes = nil
		require.Equal(t, expectedGeneration, authority.Lease.Generation)
		require.Equal(t, document.EmbeddingInputOriginalFile, authority.InputKind)
		require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), authority.Lease.ID, authority.Lease.FencingToken, now))
		var leases int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM vector_index_reader_leases`).Scan(&leases))
		require.Zero(t, leases)
	}
	t.Logf("executed SELECTs: small=%d multi=%d", counts[0], counts[1])
	require.LessOrEqual(t, counts[1], counts[0]+4, "source SQL executions must stay bounded as members grow")
}
