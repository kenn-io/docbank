package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoImportPreparationThrottlesAdmissionAndConsumesCancellation(t *testing.T) {
	ing := newTestIngester(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "video.MP4"), bytes.Repeat([]byte("synthetic video"), 600000), 0o600))
	inflight, admissions, cancellations := 0, 0, 0
	started := time.Now()
	report, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{
		ActivityBegin: func() { inflight++ },
		ActivityEnd:   func() { inflight-- },
		Admit: func(context.Context) (bool, error) {
			admissions++
			require.Zero(t, inflight, "admission may wait on a paused lane")
			return false, nil
		},
		Mutate: func(_ context.Context, fn func() error) error {
			require.Equal(t, 1, inflight)
			return fn()
		},
		Cancelled: func(context.Context) (bool, error) { cancellations++; return false, nil },
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.Receipt.Added)
	require.LessOrEqual(t, admissions, int(time.Since(started)/(100*time.Millisecond))+2)
	require.Equal(t, 2, cancellations, "only destination and group commit recheck cancellation")
	require.Zero(t, inflight)
	report, err = ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{
		Admit: func(context.Context) (bool, error) { return true, nil },
		Cancelled: func(context.Context) (bool, error) {
			t.Fatal("admission already returned cancellation")
			return false, nil
		},
	})
	require.NoError(t, err)
	require.True(t, report.Cancelled)
}

func TestPhotoImportPauseAfterGroupThenCancel(t *testing.T) {
	t.Run("after group", func(t *testing.T) {
		ing := newTestIngester(t)
		root := t.TempDir()
		for i := range 2 {
			require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("photo-%d.JPG", i)), []byte(fmt.Sprintf("image-%d", i)), 0o600))
		}
		operation, err := ing.Store.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, fmt.Sprintf(`{"source_root":%q,"destination":"/photos"}`, root))
		require.NoError(t, err)
		runner := PhotoImportRunner{Ingester: ing}
		calls := 0
		runner.Options.Mutate = func(_ context.Context, fn func() error) error {
			if err := fn(); err != nil {
				return err
			}
			calls++
			if calls == 2 {
				_, err := ing.Store.SetLaneControl(t.Context(), store.LaneControl{Lane: operation.Kind, Paused: true, Concurrency: 1}, 1)
				return err
			}
			return nil
		}
		supervisor := jobs.New(t.Context(), nil)
		defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
		require.NoError(t, runner.Start(supervisor, operation.ID))
		require.Eventually(t, func() bool {
			current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
			return err == nil && current.State == store.StorageOperationQueued && current.CompletedObjects == 1
		}, 10*time.Second, 10*time.Millisecond)
		current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
		require.NoError(t, err)
		require.Empty(t, current.Error)
		require.NoError(t, ing.Store.RequestStorageOperationCancel(t.Context(), operation.ID))
		require.Eventually(t, func() bool {
			current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
			return err == nil && current.State == store.StorageOperationCancelled && current.CompletedObjects == 1
		}, 10*time.Second, 10*time.Millisecond)
	})
	t.Run("startup", func(t *testing.T) {
		ing := newTestIngester(t)
		s := ing.Store
		runner := PhotoImportRunner{Ingester: ing}
		mutations := 0
		runner.Options.Mutate = func(_ context.Context, fn func() error) error { mutations++; return fn() }
		source := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(source, "next.JPG"), []byte("synthetic image"), 0o600))
		request := fmt.Sprintf(`{"source_root":%q,"destination":"/photos"}`, source)
		operation, err := s.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, request)
		require.NoError(t, err)
		_, err = s.ClaimStorageOperation(t.Context(), operation.ID)
		require.NoError(t, err)
		require.NoError(t, s.SetStorageOperationTotal(t.Context(), operation.ID, 3))
		const receipt = `{"added":2,"skipped":1}`
		require.NoError(t, s.AdvanceStorageOperation(t.Context(), operation.ID, "group-two", 2, 1, 42, receipt))
		_, err = s.SetLaneControl(t.Context(), store.LaneControl{Lane: operation.Kind, Paused: true, Concurrency: 1}, 1)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx, operation.ID) }()
		require.Eventually(t, func() bool {
			current, err := s.StorageOperation(t.Context(), operation.ID)
			return err == nil && current.State == store.StorageOperationQueued
		}, 10*time.Second, 10*time.Millisecond)
		require.NoError(t, s.RequestStorageOperationCancel(t.Context(), operation.ID))
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("import did not finish after admission")
		}
		current, err := s.StorageOperation(t.Context(), operation.ID)
		require.NoError(t, err)
		require.Equal(t, store.StorageOperationCancelled, current.State)
		require.Zero(t, mutations, "startup cancellation starts no new import work")
	})
}

func TestPhotoImportRetriesAdmissionFailure(t *testing.T) {
	for _, phase := range []string{"startup", "after group"} {
		t.Run(phase, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "vault.db")
			s, err := store.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			blobs, err := blob.New(store.NewPackCatalog(s), filepath.Join(home, "blobs"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, blobs.Close()) })
			ing := &Ingester{Store: s, Blobs: blobs}
			source := t.TempDir()
			for i := range 2 {
				require.NoError(t, os.WriteFile(filepath.Join(source, fmt.Sprintf("photo-%d.JPG", i)), []byte(fmt.Sprintf("synthetic image-%d", i)), 0o600))
			}
			operation, err := s.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, fmt.Sprintf(`{"source_root":%q,"destination":"/photos"}`, source))
			require.NoError(t, err)
			// Admission fails the way a transient storage operation read does until the test clears it.
			var failing atomic.Bool
			runner := PhotoImportRunner{Ingester: ing}
			runner.admit = func(ctx context.Context, metadata *store.Store, id string) (bool, error) {
				if failing.Load() {
					return false, errors.New("reading storage operation: database is locked")
				}
				return jobs.AdmitStorageOperation(ctx, metadata, id)
			}
			if phase == "startup" {
				_, err := s.ClaimStorageOperation(t.Context(), operation.ID)
				require.NoError(t, err)
				require.NoError(t, s.AdvanceStorageOperation(t.Context(), operation.ID, "", 1, 0, 0, `{"added":1}`))
				failing.Store(true)
			} else {
				calls := 0
				runner.Options.Mutate = func(_ context.Context, fn func() error) error {
					if err := fn(); err != nil {
						return err
					}
					calls++
					if calls == 2 {
						failing.Store(true)
					}
					return nil
				}
			}
			supervisor := jobs.New(t.Context(), nil)
			defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
			require.NoError(t, runner.Start(supervisor, operation.ID))
			require.Eventually(t, func() bool {
				current, err := s.StorageOperation(t.Context(), operation.ID)
				return err == nil && current.State == store.StorageOperationQueued && current.Error != ""
			}, 10*time.Second, 10*time.Millisecond)
			current, err := s.StorageOperation(t.Context(), operation.ID)
			require.NoError(t, err)
			require.Equal(t, int64(1), current.CompletedObjects)
			require.Contains(t, current.ReceiptJSON, `"added":1`)
			require.Equal(t, store.StorageOperationKindPhotoImport, current.Kind)
			require.Contains(t, current.Error, "database is locked")
			failing.Store(false)
			require.Eventually(t, func() bool {
				current, err := s.StorageOperation(t.Context(), operation.ID)
				return err == nil && current.State == store.StorageOperationCompleted
			}, 10*time.Second, 10*time.Millisecond)
		})
	}
}
