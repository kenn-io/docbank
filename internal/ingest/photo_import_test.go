package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoImportGrouping(t *testing.T) {
	root := t.TempDir()
	grouped := GroupPhotoCandidates([]PhotoImportCandidate{
		{Path: filepath.Join(root, "IMG_0001.ARW"), Kind: store.PhotoSourceRAW},
		{Path: filepath.Join(root, "IMG_0001.JPG"), Kind: store.PhotoSourceImage},
		{Path: filepath.Join(root, "IMG_0001.XMP"), Kind: store.PhotoSourceSidecar},
		{Path: filepath.Join(root, "IMG_0001.MP4"), Kind: store.PhotoSourceVideo},
	})
	require.Len(t, grouped, 2)
	assert.ElementsMatch(t, []PhotoImportCandidate{
		{Path: filepath.Join(root, "IMG_0001.ARW"), Kind: store.PhotoSourceRAW},
		{Path: filepath.Join(root, "IMG_0001.JPG"), Kind: store.PhotoSourceImage},
		{Path: filepath.Join(root, "IMG_0001.XMP"), Kind: store.PhotoSourceSidecar},
	}, grouped[0].Members)
	assert.Equal(t, []PhotoImportCandidate{{Path: filepath.Join(root, "IMG_0001.MP4"), Kind: store.PhotoSourceVideo}}, grouped[1].Members)
}

func nodeByName(t *testing.T, ing *Ingester, path string) store.Node {
	t.Helper()
	node, err := ing.Store.NodeByPath(t.Context(), path)
	require.NoError(t, err)
	return node
}

func TestPhotoImportDiscovery(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "capture.JPG")
	require.NoError(t, os.WriteFile(keep, []byte("jpeg"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".DS_Store"), []byte("junk"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("skip"), 0o600))
	folder := filepath.Join(root, ".Spotlight-V100")
	require.NoError(t, os.Mkdir(folder, 0o700))
	hidden := filepath.Join(folder, "hidden.JPG")
	require.NoError(t, os.WriteFile(hidden, []byte("photo"), 0o600))
	linkTarget := filepath.Join(root, "linked.JPG")
	require.NoError(t, os.WriteFile(linkTarget, []byte("linked"), 0o600))
	candidates, unsupported, err := discoverPhotoCandidates(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, candidates, 3)
	assert.Equal(t, int64(2), unsupported, ".DS_Store and notes.txt are counted, not imported")
	assert.ElementsMatch(t, []string{keep, linkTarget, hidden}, []string{candidates[0].Path, candidates[1].Path, candidates[2].Path})

	t.Run("descendant symlink", func(t *testing.T) {
		link := filepath.Join(root, "descendant.JPG")
		if err := os.Symlink(linkTarget, link); err != nil {
			t.Skipf("symbolic links unavailable: %v", err)
		}
		candidates, unsupported, err := discoverPhotoCandidates(t.Context(), root)
		require.NoError(t, err)
		require.Len(t, candidates, 3)
		assert.Equal(t, int64(2), unsupported)
		assert.ElementsMatch(t,
			[]string{keep, linkTarget, hidden},
			[]string{candidates[0].Path, candidates[1].Path, candidates[2].Path})
	})
	t.Run("root symlink", func(t *testing.T) {
		rootLink := filepath.Join(t.TempDir(), "camera-link")
		if err := os.Symlink(root, rootLink); err != nil {
			t.Skipf("root symbolic links unavailable: %v", err)
		}
		candidates, unsupported, err := discoverPhotoCandidates(t.Context(), rootLink)
		require.NoError(t, err)
		require.Len(t, candidates, 3)
		assert.Equal(t, int64(2), unsupported)
		expected := []string{keep, linkTarget, hidden}
		for i, path := range expected {
			expected[i], err = filepath.EvalSymlinks(path)
			require.NoError(t, err)
		}
		assert.ElementsMatch(t, expected, []string{candidates[0].Path, candidates[1].Path, candidates[2].Path})
	})
}

func TestPhotoImportSkipsGroupChangedAfterScan(t *testing.T) {
	root := t.TempDir()
	changed := filepath.Join(root, "IMG_0.JPG")
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0.ARW"), []byte("raw-0"), 0o600))
	require.NoError(t, os.WriteFile(changed, []byte("jpeg-0"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_1.JPG"), []byte("jpeg-1"), 0o600))
	ing := newTestIngester(t)
	calls := 0
	report, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{
		Mutate: func(_ context.Context, fn func() error) error {
			// Call 1 sets up the destination; call 2 is the first group, after the scan.
			if calls++; calls == 2 {
				require.NoError(t, os.WriteFile(changed, []byte("jpeg-0-edited"), 0o600))
			}
			return fn()
		},
	})
	require.NoError(t, err)
	assert.Empty(t, report.Errors)
	assert.Equal(t, int64(1), report.Receipt.Added)
	assert.Equal(t, int64(1), report.Receipt.Changed)
	assert.Zero(t, report.Receipt.Skipped)
	for _, path := range []string{"/photos/IMG_0.ARW", "/photos/IMG_0.JPG"} {
		_, err := ing.Store.NodeByPath(t.Context(), path)
		require.ErrorIs(t, err, store.ErrNotFound)
	}
	nodeByName(t, ing, "/photos/IMG_1.JPG")

	rerun, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), rerun.Receipt.Added)
	nodeByName(t, ing, "/photos/IMG_0.ARW")
	nodeByName(t, ing, "/photos/IMG_0.JPG")
}

func TestPhotoImportPublishesBytesAndPassesThroughReceipts(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "capture.JPG")
	content := []byte("published photo bytes")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	ing := newTestIngester(t)
	report, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.Receipt.Added)
	node, err := ing.Store.NodeByPath(t.Context(), "/photos/capture.JPG")
	require.NoError(t, err)
	reader, err := ing.Blobs.Open(node.BlobHash)
	require.NoError(t, err)
	got, readErr := io.ReadAll(reader)
	require.NoError(t, errors.Join(readErr, reader.Close()))
	assert.Equal(t, content, got)
	assert.Zero(t, report.Receipt.Ambiguous)
	assert.Empty(t, report.Receipt.Ambiguities)
	assert.Zero(t, report.Receipt.Skipped)
	replayed, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), replayed.Receipt.Skipped)
	assert.Zero(t, replayed.Receipt.Added)

	t.Run("ambiguity receipt pass-through", func(t *testing.T) {
		ing := newTestIngester(t)
		root := t.TempDir()
		for name, bytes := range map[string]string{"IMG_0001.ARW": "raw-one", "IMG_0001.DNG": "raw-two", "IMG_0001.JPG": "jpeg"} {
			require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(bytes), 0o600))
		}
		ambiguous, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos/ambiguous", PhotoImportOptions{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), ambiguous.Receipt.Ambiguous)
		require.Len(t, ambiguous.Receipt.Ambiguities, 1)
	})
}

func TestPhotoImportRunnerHandlesCancellationResumeAndAdmissionRetry(t *testing.T) {
	for _, interruption := range []string{"before work", "after final group", "shutdown and resume", "startup pause cancellation", "after-one-group cancellation", "startup admission failure"} {
		t.Run(interruption, func(t *testing.T) {
			ing := newTestIngester(t)
			root := t.TempDir()
			for i := range 3 {
				require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("IMG_%d.JPG", i)), []byte(fmt.Sprint("jpeg-", i)), 0o600))
			}
			operation, err := ing.Store.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport,
				fmt.Sprintf(`{"source_root":%q,"destination":"/photos"}`, root))
			require.NoError(t, err)
			runner := PhotoImportRunner{Ingester: ing}
			var mutations atomic.Int64
			var failing atomic.Bool
			var cancelAdmission atomic.Bool
			switch interruption {
			case "before work":
				require.NoError(t, ing.Store.RequestStorageOperationCancel(t.Context(), operation.ID))
			case "after final group":
				gatedCalls, importedGroups := 0, 0
				runner.Options.Mutate = func(ctx context.Context, fn func() error) error {
					if err := fn(); err != nil {
						return err
					}
					gatedCalls++
					// The first gated call creates the destination; later calls import groups.
					if gatedCalls > 1 {
						importedGroups++
					}
					if importedGroups == 3 {
						return ing.Store.RequestStorageOperationCancel(ctx, operation.ID)
					}
					return nil
				}
			case "startup pause cancellation":
				_, err = ing.Store.ClaimStorageOperation(t.Context(), operation.ID)
				require.NoError(t, err)
				require.NoError(t, ing.Store.SetStorageOperationTotal(t.Context(), operation.ID, 3))
				require.NoError(t, ing.Store.AdvanceStorageOperation(t.Context(), operation.ID, "group-two", 2, 1, 42, `{"added":2,"skipped":1}`))
				_, err = ing.Store.SetLaneControl(t.Context(), store.LaneControl{Lane: store.StorageOperationKindPhotoImport, Paused: true, Concurrency: 1}, 1)
				require.NoError(t, err)
				runner.Options.Mutate = func(_ context.Context, fn func() error) error { mutations.Add(1); return fn() }
			case "startup admission failure":
				runner.admit = func(ctx context.Context, metadata *store.Store, id string) (bool, error) {
					if failing.Load() {
						return false, errors.New("reading storage operation: database is locked")
					}
					return jobs.AdmitStorageOperation(ctx, metadata, id)
				}
				_, err = ing.Store.ClaimStorageOperation(t.Context(), operation.ID)
				require.NoError(t, err)
				require.NoError(t, ing.Store.AdvanceStorageOperation(t.Context(), operation.ID, "", 1, 0, 0, `{"added":1}`))
				failing.Store(true)
			case "after-one-group cancellation":
				runner.admit = func(context.Context, *store.Store, string) (bool, error) {
					return cancelAdmission.Load(), nil
				}
				gatedCalls, importedGroups := 0, 0
				runner.Options.Mutate = func(ctx context.Context, fn func() error) error {
					if err := fn(); err != nil {
						return err
					}
					gatedCalls++
					// The first gated call creates the destination; later calls import groups.
					if gatedCalls > 1 {
						importedGroups++
					}
					if importedGroups == 1 {
						cancelAdmission.Store(true)
					}
					return nil
				}
			case "shutdown and resume":
				stopped, stop := context.WithCancel(t.Context())
				shutdown := runner
				shutdown.Options.Mutate = func(ctx context.Context, _ func() error) error {
					stop()
					return ctx.Err()
				}
				require.ErrorIs(t, shutdown.Run(stopped, operation.ID), context.Canceled)
				resumable, err := ing.Store.ResumableStorageOperations(t.Context())
				require.NoError(t, err)
				require.Len(t, resumable, 1)
				assert.Equal(t, store.StorageOperationRunning, resumable[0].State)
			}
			if interruption == "startup pause cancellation" {
				supervisor := jobs.New(t.Context(), nil)
				defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
				require.NoError(t, runner.Start(supervisor, operation.ID))
				require.Eventually(t, func() bool {
					current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
					return err == nil && current.State == store.StorageOperationQueued
				}, 10*time.Second, 10*time.Millisecond)
				require.NoError(t, ing.Store.RequestStorageOperationCancel(t.Context(), operation.ID))
				require.Eventually(t, func() bool {
					current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
					return err == nil && current.State == store.StorageOperationCancelled
				}, 10*time.Second, 10*time.Millisecond)
			} else if strings.Contains(interruption, "admission failure") {
				supervisor := jobs.New(t.Context(), nil)
				defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
				require.NoError(t, runner.Start(supervisor, operation.ID))
				require.Eventually(t, func() bool {
					current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
					return err == nil && current.State == store.StorageOperationQueued && current.CompletedObjects > 0 && current.Error != ""
				}, 10*time.Second, 10*time.Millisecond)
				current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
				require.NoError(t, err)
				require.Equal(t, int64(1), current.CompletedObjects)
				require.Contains(t, current.ReceiptJSON, `"added":1`)
				require.Equal(t, store.StorageOperationKindPhotoImport, current.Kind)
				require.Contains(t, current.Error, "database is locked")
				failing.Store(false)
				require.Eventually(t, func() bool {
					current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
					return err == nil && current.State == store.StorageOperationCompleted
				}, 10*time.Second, 10*time.Millisecond)
			} else {
				require.NoError(t, runner.Run(t.Context(), operation.ID))
			}
			finished, err := ing.Store.StorageOperation(t.Context(), operation.ID)
			require.NoError(t, err)
			switch interruption {
			case "before work":
				assert.Equal(t, store.StorageOperationCancelled, finished.State)
				assert.Zero(t, finished.CompletedObjects)
			case "startup pause cancellation":
				assert.Equal(t, store.StorageOperationCancelled, finished.State)
				assert.Zero(t, mutations.Load(), "startup cancellation starts no new import work")
				assert.Equal(t, int64(2), finished.CompletedObjects)
				assert.Equal(t, int64(1), finished.CopiedObjects)
				assert.Equal(t, int64(42), finished.CopiedBytes)
				assert.JSONEq(t, `{"added":2,"skipped":1}`, finished.ReceiptJSON)
			case "after-one-group cancellation":
				assert.Equal(t, store.StorageOperationCancelled, finished.State)
				assert.Equal(t, int64(1), finished.CompletedObjects)
			case "startup admission failure":
				assert.Equal(t, store.StorageOperationCompleted, finished.State)
				assert.Equal(t, int64(3), finished.TotalObjects)
				assert.Equal(t, int64(3), finished.CompletedObjects)
			case "after final group":
				assert.Equal(t, store.StorageOperationCancelled, finished.State)
				assert.Equal(t, int64(3), finished.CompletedObjects)
			case "shutdown and resume":
				assert.Equal(t, store.StorageOperationCompleted, finished.State)
				assert.Equal(t, int64(3), finished.TotalObjects)
				assert.Equal(t, int64(3), finished.CompletedObjects)
				assert.JSONEq(t, `{"added":3,"skipped":0,"changed":0,"failed":0,"ambiguous":0,"unsupported":0}`, finished.ReceiptJSON)
			}
		})
	}
}
