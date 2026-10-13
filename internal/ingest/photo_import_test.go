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

func TestPhotoImportAmbiguity(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.ARW"), []byte("raw-one"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.DNG"), []byte("raw-two"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.JPG"), []byte("jpeg"), 0o600))
	ing := newTestIngester(t)
	first, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), first.Receipt.Ambiguous)
	require.Len(t, first.Receipt.Ambiguities, 1)
	ambiguity := first.Receipt.Ambiguities[0]
	assert.Equal(t, store.PhotoImportMultipleRAW, ambiguity.Reason)
	require.Len(t, ambiguity.Files, 3)

	repeated, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), repeated.Receipt.Ambiguous)
	assert.Zero(t, repeated.Receipt.Added)
}

func TestPhotoImportLateJPEGJoinsEarlierRAW(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.ARW"), []byte("raw"), 0o600))
	ing := newTestIngester(t)
	first, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(1), first.Receipt.Added)
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.JPG"), []byte("jpeg"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.XMP"), []byte("xmp"), 0o600))
	second, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), second.Receipt.Added)
	asset, err := ing.Store.PhotoAssetForNode(t.Context(), nodeByName(t, ing, "/photos/IMG_0001.JPG").ID)
	require.NoError(t, err)
	assert.Len(t, asset.Files, 3)
	raw, err := ing.Store.PhotoAssetForNode(t.Context(), nodeByName(t, ing, "/photos/IMG_0001.ARW").ID)
	require.NoError(t, err)
	assert.Equal(t, raw.ID, asset.ID)

	third, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), third.Receipt.Skipped)
	assert.Zero(t, third.Receipt.Added)
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
		assert.ElementsMatch(t, []string{keep, linkTarget, hidden}, []string{candidates[0].Path, candidates[1].Path, candidates[2].Path})
	})
	t.Run("root symlink", func(t *testing.T) {
		rootLink := filepath.Join(t.TempDir(), "camera-link")
		if err := os.Symlink(root, rootLink); err != nil {
			t.Skipf("root symbolic links unavailable: %v", err)
		}
		candidates, _, err := discoverPhotoCandidates(t.Context(), rootLink)
		require.NoError(t, err)
		assert.NotEmpty(t, candidates)
	})
}

func TestPhotoImportSkipsGroupChangedAfterScan(t *testing.T) {
	root := t.TempDir()
	changed := filepath.Join(root, "IMG_0.JPG")
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

	rerun, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), rerun.Receipt.Added)
	nodeByName(t, ing, "/photos/IMG_0.JPG")
}

func TestPhotoImportPublishedBytes(t *testing.T) {
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
	assert.Equal(t, got, content)
}

func TestPhotoImportSettlesOnceOutsideTheGate(t *testing.T) {
	root := t.TempDir()
	for i := range 6 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("IMG_%d.JPG", i)), []byte(fmt.Sprint("jpeg-", i)), 0o600))
	}
	ing := newTestIngester(t)
	begin, end := 0, 0
	interval := 500 * time.Millisecond
	var entered, left []time.Time
	started := time.Now()
	report, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{
		SettleInterval: interval,
		ActivityBegin:  func() { begin++ },
		ActivityEnd:    func() { end++ },
		Mutate: func(_ context.Context, fn func() error) error {
			entered = append(entered, time.Now())
			defer func() { left = append(left, time.Now()) }()
			return fn()
		},
	})
	elapsed := time.Since(started)
	require.NoError(t, err)
	assert.Equal(t, int64(6), report.Receipt.Added)
	assert.Equal(t, 1, begin)
	assert.Equal(t, 1, end)
	require.Len(t, entered, 7)
	// The only wait sits between destination setup and the first group, outside the gate.
	assert.GreaterOrEqual(t, entered[1].Sub(left[0]), interval)
	var held time.Duration
	for i := range entered {
		held += left[i].Sub(entered[i])
	}
	assert.Less(t, elapsed-held, 6*interval)
}
func TestPhotoImportRunnerCancelsAndResumes(t *testing.T) {
	for _, interruption := range []string{"before work", "after final group", "shutdown and resume", "startup pause cancellation", "after-one-group cancellation", "startup admission failure", "after-group admission failure"} {
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
			switch interruption {
			case "before work":
				require.NoError(t, ing.Store.RequestStorageOperationCancel(t.Context(), operation.ID))
			case "after final group":
				runner.Options.Mutate = func(ctx context.Context, fn func() error) error {
					if err := fn(); err != nil {
						return err
					}
					if _, err := ing.Store.NodeByPath(ctx, "/photos/IMG_2.JPG"); err == nil {
						return ing.Store.RequestStorageOperationCancel(ctx, operation.ID)
					}
					return nil
				}
			case "startup pause cancellation":
				_, err = ing.Store.ClaimStorageOperation(t.Context(), operation.ID)
				require.NoError(t, err)
				require.NoError(t, ing.Store.SetStorageOperationTotal(t.Context(), operation.ID, 3))
				require.NoError(t, ing.Store.AdvanceStorageOperation(t.Context(), operation.ID, "group-two", 2, 1, 42, `{"added":2,"skipped":1}`))
				_, err = ing.Store.SetLaneControl(t.Context(), store.LaneControl{Lane: operation.Kind, Paused: true, Concurrency: 1}, 1)
				require.NoError(t, err)
				runner.Options.Mutate = func(_ context.Context, fn func() error) error { mutations.Add(1); return fn() }
			case "startup admission failure", "after-group admission failure":
				runner.admit = func(ctx context.Context, metadata *store.Store, id string) (bool, error) {
					if failing.Load() {
						return false, errors.New("reading storage operation: database is locked")
					}
					return jobs.AdmitStorageOperation(ctx, metadata, id)
				}
				if interruption == "startup admission failure" {
					_, err = ing.Store.ClaimStorageOperation(t.Context(), operation.ID)
					require.NoError(t, err)
					require.NoError(t, ing.Store.AdvanceStorageOperation(t.Context(), operation.ID, "", 1, 0, 0, `{"added":1}`))
					failing.Store(true)
				}
				fallthrough
			case "after-one-group cancellation":
				deferred := false
				runner.Options.Mutate = func(ctx context.Context, fn func() error) error {
					if err := fn(); err != nil {
						return err
					}
					if _, err := ing.Store.NodeByPath(ctx, "/photos/IMG_0.JPG"); err == nil && !deferred {
						deferred = true
						if interruption == "after-one-group cancellation" {
							_, err = ing.Store.SetLaneControl(ctx, store.LaneControl{Lane: operation.Kind, Paused: true, Concurrency: 1}, 1)
							return err
						}
						if interruption == "after-group admission failure" {
							failing.Store(true)
						}
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
			if strings.Contains(interruption, "pause cancellation") || interruption == "after-one-group cancellation" || strings.Contains(interruption, "admission failure") {
				supervisor := jobs.New(t.Context(), nil)
				defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
				require.NoError(t, runner.Start(supervisor, operation.ID))
				require.Eventually(t, func() bool {
					current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
					return err == nil && current.State == store.StorageOperationQueued && current.CompletedObjects > 0 && (current.Error != "" || !strings.Contains(interruption, "admission failure"))
				}, 10*time.Second, 10*time.Millisecond)
				current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
				require.NoError(t, err)
				if strings.Contains(interruption, "admission failure") {
					require.Equal(t, int64(1), current.CompletedObjects)
					require.Contains(t, current.ReceiptJSON, `"added":1`)
					require.Equal(t, store.StorageOperationKindPhotoImport, current.Kind)
					require.Contains(t, current.Error, "database is locked")
					failing.Store(false)
				} else {
					require.Empty(t, current.Error)
					require.NoError(t, ing.Store.RequestStorageOperationCancel(t.Context(), operation.ID))
				}
				require.Eventually(t, func() bool {
					current, err := ing.Store.StorageOperation(t.Context(), operation.ID)
					return err == nil && (current.State == store.StorageOperationCompleted || current.State == store.StorageOperationCancelled)
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
			case "startup admission failure", "after-group admission failure":
				assert.Equal(t, store.StorageOperationCompleted, finished.State)
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
