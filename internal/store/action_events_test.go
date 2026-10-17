package store

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImportClaimsSurviveReopenAndReportOnce(t *testing.T) {
	t.Parallel()
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			s, err := Open(path, test.driver)
			require.NoError(t, err)
			request := `{"source_root":"/camera","destination":"/photos"}`
			started, err := s.CreatePhotoImport(ctx, request, "cli")
			require.NoError(t, err)
			scripted, err := s.CreatePhotoImport(ctx, request, "")
			require.NoError(t, err)
			_, err = s.CreatePhotoImport(ctx, request, "agent")
			require.ErrorContains(t, err, `unsupported import action surface "agent"`)
			require.NoError(t, s.Close())

			s, err = Open(path, test.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			surface, err := s.ConsumeImportClaim(ctx, PhotoImportOwner, started.ID)
			require.NoError(t, err)
			require.Equal(t, "cli", surface, "the origin survives a restart")
			surface, err = s.ConsumeImportClaim(ctx, PhotoImportOwner, started.ID)
			require.NoError(t, err)
			require.Empty(t, surface, "a consumed claim reports nothing")
			surface, err = s.ConsumeImportClaim(ctx, PhotoImportOwner, scripted.ID)
			require.NoError(t, err)
			require.Empty(t, surface)
			_, err = s.db.ExecContext(ctx, `DELETE FROM storage_operations WHERE operation_id=?`, started.ID)
			require.NoError(t, err)
			var remaining int
			require.NoError(t, s.db.QueryRowContext(ctx,
				`SELECT count(*) FROM import_action_claims WHERE operation_id=?`, started.ID).Scan(&remaining))
			require.Zero(t, remaining, "deleting the owner releases its local claim")

			const runID = "00000000-0000-4000-8000-000000000001"
			_, err = s.db.ExecContext(ctx, `INSERT INTO ingests(id,started_at,source_kind,source_desc)
				VALUES(?,?,'push','synthetic')`, runID, nowRFC3339())
			require.NoError(t, err)
			var claims atomic.Int64
			var wg sync.WaitGroup
			failures := make(chan error, 8)
			for range 8 {
				wg.Go(func() {
					claimed, err := s.ClaimPushRun(ctx, runID, "cli")
					failures <- err
					if claimed {
						claims.Add(1)
					}
				})
			}
			wg.Wait()
			close(failures)
			for err := range failures {
				require.NoError(t, err)
			}
			require.Equal(t, int64(1), claims.Load(), "concurrent uploads in one run share one claim")
			_, err = s.ClaimPushRun(ctx, "00000000-0000-4000-8000-000000000002", "cli")
			require.Error(t, err, "a run claims only after it has added a document")
		})
	}
}
