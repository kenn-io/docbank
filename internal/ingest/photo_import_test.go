package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Len(t, grouped[0].Members, 3)
	assert.Len(t, grouped[1].Members, 1)
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
	for _, name := range []string{"IMG_0001.ARW", "IMG_0001.DNG", "IMG_0001.JPG"} {
		asset, err := ing.Store.PhotoAssetForNode(t.Context(), nodeByName(t, ing, "/photos/"+name).ID)
		require.NoError(t, err, name)
		assert.Len(t, asset.Files, 1, name)
	}

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
	link := filepath.Join(root, "descendant.JPG")
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	candidates, unsupported, err := discoverPhotoCandidates(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, candidates, 3)
	assert.Equal(t, int64(2), unsupported, ".DS_Store and notes.txt are counted, not imported")
	assert.ElementsMatch(t, []string{keep, linkTarget, hidden}, []string{candidates[0].Path, candidates[1].Path, candidates[2].Path})

	rootLink := filepath.Join(t.TempDir(), "camera-link")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Skipf("root symbolic links unavailable: %v", err)
	}
	candidates, _, err = discoverPhotoCandidates(t.Context(), rootLink)
	require.NoError(t, err)
	assert.NotEmpty(t, candidates)
}

func TestPhotoImportSourceStability(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	for _, test := range []struct {
		name string
		edit func(t *testing.T, path string, before localFileFingerprint)
	}{
		{name: "size changed", edit: func(t *testing.T, path string, _ localFileFingerprint) {
			t.Helper()
			require.NoError(t, os.WriteFile(path, []byte("changed-size"), 0o600))
		}},
		{name: "same-size replacement", edit: func(t *testing.T, path string, before localFileFingerprint) {
			t.Helper()
			require.NoError(t, os.Rename(path, path+".old"))
			require.NoError(t, os.WriteFile(path, []byte("same"), 0o600))
			require.NoError(t, os.Chtimes(path, before.info.ModTime(), before.info.ModTime()))
		}},
		{name: "disappeared", edit: func(t *testing.T, path string, _ localFileFingerprint) {
			t.Helper()
			require.NoError(t, os.Remove(path))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture.JPG")
			require.NoError(t, os.WriteFile(path, []byte("same"), 0o600))
			before, err := observeLocalFileFingerprint(path)
			require.NoError(t, err)
			test.edit(t, path, before)
			_, err = ing.readLocalFile(ctx, path, path, nil, &before)
			require.Error(t, err)
			if test.name != "disappeared" {
				assert.ErrorIs(t, err, ErrSourceChanged)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "capture.JPG")
	other := filepath.Join(t.TempDir(), "other.JPG")
	require.NoError(t, os.WriteFile(path, []byte("same"), 0o600))
	require.NoError(t, os.WriteFile(other, []byte("other"), 0o600))
	before, err := observeLocalFileFingerprint(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(other, path))
	_, err = ing.readLocalFile(ctx, path, path, nil, &before)
	require.Error(t, err)
	assert.Error(t, err)
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

func TestPhotoImportAuditedVaultDoesNotCreateDestination(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	audited, err := ing.Store.Mkdir(ctx, ing.Store.RootID(), "audited")
	require.NoError(t, err)
	plan, err := ing.Store.PreviewInitialAudit(ctx, audited.ID, "api", nil)
	require.NoError(t, err)
	_, err = ing.Store.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "IMG.JPG"), []byte("synthetic-jpeg"), 0o600))
	_, err = ing.ImportPhotoDirectory(ctx, source, "/audited/new/nested", PhotoImportOptions{})
	require.ErrorIs(t, err, store.ErrAuditMutationUnsupported)
	_, err = ing.Store.NodeByPath(ctx, "/audited/new")
	require.ErrorIs(t, err, store.ErrNotFound)
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
func TestPhotoImportRunnerHonorsCancelAfterLastGroup(t *testing.T) {
	ing := newTestIngester(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0.JPG"), []byte("jpeg"), 0o600))
	operation, err := ing.Store.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport,
		fmt.Sprintf(`{"source_root":%q,"destination":"/photos"}`, root))
	require.NoError(t, err)
	runner := PhotoImportRunner{Ingester: ing}
	calls := 0
	// The second gated call commits the only group; a cancel right after it races the finish.
	runner.Options.Mutate = func(ctx context.Context, fn func() error) error {
		if err := fn(); err != nil {
			return err
		}
		calls++
		if calls == 2 {
			return ing.Store.RequestStorageOperationCancel(ctx, operation.ID)
		}
		return nil
	}
	require.NoError(t, runner.Run(t.Context(), operation.ID))
	finished, err := ing.Store.StorageOperation(t.Context(), operation.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCancelled, finished.State)
	assert.Equal(t, int64(1), finished.CompletedObjects)
}

func TestPhotoImportRunnerCancelsAndResumes(t *testing.T) {
	ing := newTestIngester(t)
	root := t.TempDir()
	for i := range 3 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("IMG_%d.JPG", i)), []byte(fmt.Sprint("jpeg-", i)), 0o600))
	}
	request := fmt.Sprintf(`{"source_root":%q,"destination":"/photos"}`, root)
	runner := PhotoImportRunner{Ingester: ing}

	stopped, stop := context.WithCancel(t.Context())
	shutdown := runner
	shutdown.Options.Mutate = func(ctx context.Context, _ func() error) error {
		stop()
		return ctx.Err()
	}
	interrupted, err := ing.Store.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, request)
	require.NoError(t, err)
	require.ErrorIs(t, shutdown.Run(stopped, interrupted.ID), context.Canceled)
	resumable, err := ing.Store.ResumableStorageOperations(t.Context())
	require.NoError(t, err)
	require.Len(t, resumable, 1)
	assert.Equal(t, store.StorageOperationRunning, resumable[0].State)
	require.NoError(t, runner.Run(t.Context(), interrupted.ID))
	completed, err := ing.Store.StorageOperation(t.Context(), interrupted.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCompleted, completed.State)
	assert.Equal(t, int64(3), completed.TotalObjects)
	assert.Equal(t, int64(3), completed.CompletedObjects)
	assert.JSONEq(t, `{"added":3,"skipped":0,"changed":0,"failed":0,"ambiguous":0,"unsupported":0}`, completed.ReceiptJSON)

	cancelled, err := ing.Store.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, request)
	require.NoError(t, err)
	require.NoError(t, ing.Store.RequestStorageOperationCancel(t.Context(), cancelled.ID))
	require.NoError(t, runner.Run(t.Context(), cancelled.ID))
	finished, err := ing.Store.StorageOperation(t.Context(), cancelled.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StorageOperationCancelled, finished.State)
	assert.Zero(t, finished.CompletedObjects)
}
