package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
)

func seedExportMembers(t *testing.T, s *Store, start, end int) []bundle.Member {
	t.Helper()
	members := make([]bundle.Member, 0, end-start)
	for lower := start; lower < end; lower += 1000 {
		upper := min(lower+1000, end)
		require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
			for index := lower; index < upper; index++ {
				n, _, err := s.createFileTx(t.Context(), tx, s.RootID(), fmt.Sprintf("synthetic-export-%06d.txt", index), fakeHash("e1"), 12, "text/plain")
				if err != nil {
					return err
				}
				members = append(members, bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size})
			}
			return nil
		}))
	}
	return members
}

func TestExportUpload1001MembersRequiresEveryChunkAndExactDigest(t *testing.T) {
	s := newTestStore(t)
	members := seedExportMembers(t, s, 0, 1001)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "upload", Total: len(members), MemberHash: ExportMemberHash(members)}, nil)
	require.NoError(t, err)
	require.NoError(t, s.PutExportChunk(t.Context(), "owner", source.ID, 0, members[:1000]))
	_, err = s.SealExportSource(t.Context(), "owner", source.ID)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.NoError(t, s.PutExportChunk(t.Context(), "owner", source.ID, 0, members[:1000]))
	require.NoError(t, s.PutExportChunk(t.Context(), "owner", source.ID, 1, members[1000:]))
	sealed, err := s.SealExportSource(t.Context(), "owner", source.ID)
	require.NoError(t, err)
	require.Equal(t, 1001, sealed.Total)
	require.Equal(t, source.MemberHash, sealed.MemberHash)
	_, err = s.db.ExecContext(t.Context(), `UPDATE export_sources SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=?`, source.ID)
	require.NoError(t, err)
	require.NoError(t, s.CleanupExportAuthority(t.Context()))
	var retained int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM export_members WHERE source_id=?`, source.ID).Scan(&retained))
	require.Zero(t, retained)
}

func TestExportScale(t *testing.T) {
	if os.Getenv("DOCBANK_EXPORT_SCALE") != "1" {
		t.Skip("set DOCBANK_EXPORT_SCALE=1 for 25k/118k export admission measurements")
	}
	s := newTestStore(t)
	t.Logf("go=%s os=%s arch=%s cpus=%d driver=%T workload=synthetic_text source_bytes=12 source_hashes=1 batch=1000", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), s.driver)
	started := time.Now()
	seedExportMembers(t, s, 0, 25000)
	t.Logf("seed_count=25000 elapsed=%s", time.Since(started))
	service := NewQuerySnapshotService(s)
	defer func() { require.NoError(t, service.Close()) }()
	query := snapshotTestQuery(t, `{}`)
	for _, phase := range []string{"first_after_seed", "repeat"} {
		start := time.Now()
		request := bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "query", Query: &query}
		source, err := s.CreateExportSource(t.Context(), "owner", request, func(ctx context.Context) ([]bundle.Member, error) {
			page, err := service.Create(ctx, "owner", SnapshotRequest{Query: query})
			if err != nil {
				return nil, err
			}
			rows, err := service.CopyMembersBounded(ctx, "owner", page.SnapshotID, page.MemberHash, bundle.MaxMembers)
			if err != nil {
				return nil, err
			}
			members := make([]bundle.Member, len(rows))
			for i, m := range rows {
				members[i] = bundle.Member{NodeID: m.NodeID, VersionID: m.ContentVersionID, SHA256: m.BlobHash, Size: m.Size}
			}
			return members, nil
		})
		require.NoError(t, err)
		require.Equal(t, 25000, source.Total)
		sourceElapsed := time.Since(start)
		start = time.Now()
		plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
		require.NoError(t, err)
		require.Equal(t, 25000, plan.Total)
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		t.Logf("phase=%s members=25000 source_elapsed=%s plan_elapsed=%s role_bytes=%d metadata_bytes=%d heap_alloc=%d runtime_sys=%d", phase, sourceElapsed, time.Since(start), plan.RoleBytes, plan.MetadataBytes, memory.HeapAlloc, memory.Sys)
	}
	started = time.Now()
	seedExportMembers(t, s, 25000, 118000)
	t.Logf("seed_count=118000 added=93000 elapsed=%s", time.Since(started))
	start := time.Now()
	page, err := service.Create(t.Context(), "oversize", SnapshotRequest{Query: query})
	require.NoError(t, err)
	require.Equal(t, int64(118000), page.Total)
	members, err := service.CopyMembersBounded(t.Context(), "oversize", page.SnapshotID, page.MemberHash, bundle.MaxMembers)
	require.ErrorIs(t, err, ErrQuerySnapshotTooLarge)
	require.Nil(t, members)
	t.Logf("phase=oversize members=118000 outcome=explicit_refusal elapsed=%s", time.Since(start))
}
