package store

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSavedQueryRunUsesSavedDefinitionAndRetainsComparisonReceipts(t *testing.T) {
	for _, driverCase := range walkTestDrivers() {
		t.Run(driverCase.name, func(t *testing.T) {
			s := newTestStoreWithDriver(t, driverCase.driver)
			_, err := s.CreateFile(t.Context(), s.RootID(), "first.txt", fakeHash("saved-run-first"), 7, "text/plain")
			require.NoError(t, err)
			definition, err := s.CreateSavedQuery(t.Context(), "Everything", "", SavedQueryKindQuery, []byte(`{}`))
			require.NoError(t, err)

			now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
			service := newQuerySnapshotService(s, querySnapshotServiceOptions{
				Now: func() time.Time {
					result := now
					now = now.Add(time.Nanosecond)
					return result
				},
				Rand: bytes.NewReader(append(append(bytes.Repeat([]byte{0x41}, snapshotIDBytes),
					bytes.Repeat([]byte{0x42}, snapshotIDBytes)...), bytes.Repeat([]byte{0x43}, snapshotIDBytes)...)),
				HMACKey: bytes.Repeat([]byte{0x42}, 32),
			})
			t.Cleanup(func() { require.NoError(t, service.Close()) })

			first, firstPage, err := service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision,
				SnapshotRequest{Query: snapshotTestQuery(t, `{"text":"must-not-override"}`)})
			require.NoError(t, err)
			assert.Equal(t, int64(1), first.Total)
			assert.Equal(t, first.SnapshotID, firstPage.SnapshotID)
			assert.Equal(t, definition.Fingerprint, first.QueryFingerprint)
			assert.Empty(t, first.PreviousRunID)
			assert.Equal(t, SavedQueryRunComparison{}, first.Comparison)

			_, err = s.CreateFile(t.Context(), s.RootID(), "second.txt", fakeHash("saved-run-second"), 11, "text/plain")
			require.NoError(t, err)
			second, _, err := service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
			require.NoError(t, err)
			assert.Equal(t, first.RunID, second.PreviousRunID)
			assert.False(t, second.Comparison.DefinitionChanged)
			assert.True(t, second.Comparison.HashChanged)
			assert.Equal(t, int64(1), second.Comparison.TotalDelta)
			updated, err := s.UpdateSavedQuery(t.Context(), definition.ID, definition.Revision,
				SavedQueryPatch{Payload: new([]byte(`{"sort":{"direction":"desc","field":"name"}}`))})
			require.NoError(t, err)
			third, _, err := service.RunSaved(t.Context(), "owner", updated.ID, updated.Revision, SnapshotRequest{})
			require.NoError(t, err)
			assert.Equal(t, second.RunID, third.PreviousRunID)
			assert.True(t, third.Comparison.DefinitionChanged)
			assert.False(t, third.Comparison.HashChanged)
			assert.Zero(t, third.Comparison.TotalDelta)

			runs, err := s.ListSavedQueryRuns(t.Context(), definition.ID, 100)
			require.NoError(t, err)
			require.Equal(t, []SavedQueryRun{third, second, first}, runs)
			_, err = s.ListSavedQueryRuns(t.Context(), definition.ID, 0)
			require.ErrorIs(t, err, ErrInvalidSavedQueryRun)
			_, err = s.ListSavedQueryRuns(t.Context(), definition.ID, 101)
			require.ErrorIs(t, err, ErrInvalidSavedQueryRun)
		})
	}
}

func TestSavedQueryRunRejectsStaleDefinitionAndChangedDependencyWithoutReceiptOrHandle(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(*testing.T, *Store) (SavedQuery, func(*testing.T, *Store))
	}{
		{
			name: "root definition",
			create: func(t *testing.T, s *Store) (SavedQuery, func(*testing.T, *Store)) {
				t.Helper()
				root, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
				require.NoError(t, err)
				return root, func(t *testing.T, s *Store) {
					t.Helper()
					_, err := s.UpdateSavedQuery(t.Context(), root.ID, root.Revision,
						SavedQueryPatch{Description: new("changed")})
					require.NoError(t, err)
				}
			},
		},
		{
			name: "nested dependency",
			create: func(t *testing.T, s *Store) (SavedQuery, func(*testing.T, *Store)) {
				t.Helper()
				child, err := s.CreateSavedQuery(t.Context(), "child", "", SavedQueryKindQuery, []byte(`{}`))
				require.NoError(t, err)
				root, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery,
					[]byte(`{"syntax":"advanced","text":"saved:child"}`))
				require.NoError(t, err)
				return root, func(t *testing.T, s *Store) {
					t.Helper()
					_, err := s.UpdateSavedQuery(t.Context(), child.ID, child.Revision,
						SavedQueryPatch{Description: new("changed")})
					require.NoError(t, err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			_, err := s.CreateFile(t.Context(), s.RootID(), "member.txt", fakeHash("saved-run-stale"), 1, "text/plain")
			require.NoError(t, err)
			definition, mutate := test.create(t, s)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			service := newQuerySnapshotService(s, querySnapshotServiceOptions{
				HMACKey: bytes.Repeat([]byte{0x43}, 32),
				ChargeHook: func(ctx context.Context, _ string, rows, _ int64) error {
					if rows == 0 {
						return nil
					}
					entered <- struct{}{}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			result := make(chan error, 1)
			go func() {
				_, _, runErr := service.RunSaved(context.Background(), "owner", definition.ID,
					definition.Revision, SnapshotRequest{})
				result <- runErr
			}()
			<-entered
			mutate(t, s)
			close(release)
			require.ErrorIs(t, <-result, ErrStaleRevision)
			runs, listErr := s.ListSavedQueryRuns(t.Context(), definition.ID, 10)
			require.NoError(t, listErr)
			assert.Empty(t, runs)
			assert.Equal(t, snapshotCacheStats{}, service.stats())
		})
	}
}

func TestSavedQueryRunConcurrentReceiptsFormOnePreviousRunChain(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "member.txt", fakeHash("saved-run-concurrent"), 1, "text/plain")
	require.NoError(t, err)
	definition, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{0x44}, 32)})
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	start := make(chan struct{})
	results := make(chan SavedQueryRun, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			run, _, runErr := service.RunSaved(context.Background(), "owner", definition.ID,
				definition.Revision, SnapshotRequest{})
			results <- run
			errs <- runErr
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for runErr := range errs {
		require.NoError(t, runErr)
	}
	runs, err := s.ListSavedQueryRuns(t.Context(), definition.ID, 10)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	ids := map[string]bool{runs[0].RunID: true, runs[1].RunID: true}
	linked := 0
	for run := range results {
		if run.PreviousRunID != "" {
			linked++
			assert.True(t, ids[run.PreviousRunID])
			assert.NotEqual(t, run.RunID, run.PreviousRunID)
		}
	}
	assert.Equal(t, 1, linked)
}

func TestSavedQueryRunConcurrentCommitOrderChoosesPreviousChainHead(t *testing.T) {
	s := newTestStore(t)
	definition, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var finalizers atomic.Int64
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{
		HMACKey: bytes.Repeat([]byte{0x46}, 32),
		SavedRunBeforeCommitHook: func(context.Context) error {
			if finalizers.Add(1) == 1 {
				close(firstEntered)
				<-releaseFirst
			}
			return nil
		},
	})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	type result struct {
		run SavedQueryRun
		err error
	}
	firstResult := make(chan result, 1)
	go func() {
		run, _, runErr := service.RunSaved(context.Background(), "owner-a", definition.ID,
			definition.Revision, SnapshotRequest{})
		firstResult <- result{run: run, err: runErr}
	}()
	<-firstEntered
	second, _, err := service.RunSaved(t.Context(), "owner-b", definition.ID,
		definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	close(releaseFirst)
	first := <-firstResult
	require.NoError(t, first.err)
	assert.Equal(t, second.RunID, first.run.PreviousRunID)

	third, _, err := service.RunSaved(t.Context(), "owner-c", definition.ID,
		definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	assert.Equal(t, first.run.RunID, third.PreviousRunID,
		"the next receipt must follow commit order even when materialization timestamps were inverted")
	runs, err := s.ListSavedQueryRuns(t.Context(), definition.ID, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{third.RunID, first.run.RunID, second.RunID},
		[]string{runs[0].RunID, runs[1].RunID, runs[2].RunID})
}

func TestSavedQueryRunCanceledAdmissionAndDefinitionDeletionLeaveNoAuthority(t *testing.T) {
	s := newTestStore(t)
	definition, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
	require.NoError(t, err)
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{HMACKey: bytes.Repeat([]byte{0x45}, 32)})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = service.RunSaved(canceled, "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.ErrorIs(t, err, context.Canceled)
	runs, err := s.ListSavedQueryRuns(t.Context(), definition.ID, 10)
	require.NoError(t, err)
	assert.Empty(t, runs)

	run, _, err := service.RunSaved(t.Context(), "owner", definition.ID, definition.Revision, SnapshotRequest{})
	require.NoError(t, err)
	_, err = s.DeleteSavedQuery(t.Context(), definition.ID, definition.Revision)
	require.NoError(t, err)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM saved_query_runs WHERE run_id=?`, run.RunID).Scan(&count))
	assert.Zero(t, count, "deleting the definition must cascade its receipts")
}

func TestSavedQueryRunFinalizationCancellationBoundaries(t *testing.T) {
	t.Run("revocation before commit rolls back receipt and reservation", func(t *testing.T) {
		s := newTestStore(t)
		definition, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
		require.NoError(t, err)
		entered := make(chan struct{})
		service := newQuerySnapshotService(s, querySnapshotServiceOptions{
			HMACKey: bytes.Repeat([]byte{0x61}, 32),
			SavedRunBeforeCommitHook: func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			},
		})
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		result := make(chan error, 1)
		go func() {
			_, _, runErr := service.RunSaved(context.Background(), "owner", definition.ID,
				definition.Revision, SnapshotRequest{})
			result <- runErr
		}()
		<-entered
		service.Revoke("owner")
		require.ErrorIs(t, <-result, context.Canceled)
		runs, err := s.ListSavedQueryRuns(t.Context(), definition.ID, 10)
		require.NoError(t, err)
		assert.Empty(t, runs)
		assert.Equal(t, snapshotCacheStats{}, service.stats())
	})

	t.Run("shutdown after commit is bounded and leaves receipt gone", func(t *testing.T) {
		s := newTestStore(t)
		definition, err := s.CreateSavedQuery(t.Context(), "root", "", SavedQueryKindQuery, []byte(`{}`))
		require.NoError(t, err)
		committed := make(chan struct{})
		release := make(chan struct{})
		service := newQuerySnapshotService(s, querySnapshotServiceOptions{
			HMACKey: bytes.Repeat([]byte{0x62}, 32),
			SavedRunCommittedHook: func() {
				close(committed)
				<-release
			},
		})
		result := make(chan struct {
			run  SavedQueryRun
			page SnapshotPage
			err  error
		}, 1)
		go func() {
			run, page, runErr := service.RunSaved(context.Background(), "owner", definition.ID,
				definition.Revision, SnapshotRequest{})
			result <- struct {
				run  SavedQueryRun
				page SnapshotPage
				err  error
			}{run, page, runErr}
		}()
		<-committed
		runs, err := s.ListSavedQueryRuns(t.Context(), definition.ID, 10)
		require.NoError(t, err)
		require.Len(t, runs, 1, "receipt is durable before cache finalization")
		shutdownCtx, cancel := context.WithCancel(context.Background())
		cancel()
		require.ErrorIs(t, service.Shutdown(shutdownCtx), context.Canceled)
		close(release)
		got := <-result
		require.NoError(t, got.err)
		assert.Equal(t, runs[0], got.run)
		assert.Equal(t, got.run.SnapshotID, got.page.SnapshotID)
		require.NoError(t, service.Close())
		_, err = service.Page(t.Context(), "owner", got.run.SnapshotID, "")
		require.ErrorIs(t, err, ErrSnapshotGone)
	})
}
