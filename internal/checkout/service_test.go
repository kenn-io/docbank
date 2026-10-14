package checkout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func checkoutService(t *testing.T) Service {
	t.Helper()
	vault := t.TempDir()
	catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	return Service{Store: catalog, Blobs: blobs, VaultRoot: vault, Config: config.Default()}
}

func checkoutPhoto(t *testing.T, s Service, name, content string) store.PhotoAsset {
	t.Helper()
	hash, size, err := s.Blobs.Write(strings.NewReader(content))
	require.NoError(t, err)
	node, err := s.Store.CreateFile(t.Context(), s.Store.RootID(), name, hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.Store.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	return asset
}

func TestCheckoutDestinationSafety(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}
	storageRoot, watchRoot := t.TempDir(), t.TempDir()
	s.Config.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: storageRoot}}
	s.Config.Watches = []config.WatchConfig{{Source: watchRoot}}
	reject := func(destination string) {
		t.Helper()
		_, _, root, err := s.Prepare(t.Context(), selection, destination, 0)
		if root != nil {
			require.NoError(t, root.Close())
		}
		require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	}
	for _, base := range []string{s.VaultRoot, storageRoot, watchRoot} {
		destination := filepath.Join(base, "checkout")
		require.NoError(t, os.Mkdir(destination, 0o755))
		reject(destination)
	}
	nonempty := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(nonempty, "keep"), nil, 0o600))
	reject(nonempty)
	reject("relative")
	reject(filepath.Join(t.TempDir(), "missing"))
	c, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	sub := filepath.Join(c.Destination, "nested")
	require.NoError(t, os.Mkdir(sub, 0o755))
	reject(sub)
	require.NoError(t, root.Close())
	missing := filepath.Join(t.TempDir(), "missing")
	s.Config.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: filepath.Join(missing, "archive")}}
	s.Config.Watches = []config.WatchConfig{{Source: filepath.Join(missing, "watch")}}
	_, _, root, err = s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	t.Run("canonical symlink destination", func(t *testing.T) {
		destination := t.TempDir()
		alias := filepath.Join(t.TempDir(), "checkout")
		if err := os.Symlink(destination, alias); err != nil {
			t.Skipf("symbolic links unavailable: %v", err)
		}
		canonical, err := filepath.EvalSymlinks(alias)
		require.NoError(t, err)
		created, _, root, err := s.Prepare(t.Context(), selection, alias, 0)
		require.NoError(t, err)
		defer func() { require.NoError(t, root.Close()) }()
		require.Equal(t, canonical, created.Destination)
	})
	t.Run("Run rechecks destination after Prepare", func(t *testing.T) {
		c, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
		require.NoError(t, err)
		require.NoError(t, root.Close())
		require.NoError(t, os.WriteFile(filepath.Join(c.Destination, "keep"), []byte("keep"), 0600))
		err = s.Run(t.Context(), c, nil)
		status, statusErr := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
		require.NoError(t, statusErr)
		require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
		require.Equal(t, "failed", status.Checkout.State)
		require.Zero(t, status.PublishedFiles)
		content, err := os.ReadFile(filepath.Join(c.Destination, "keep"))
		require.NoError(t, err)
		require.Equal(t, "keep", string(content))
	})
}

func TestCheckoutStartFailurePersistsFailureAndClosesRoot(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"nil supervisor", "stopped supervisor"} {
		t.Run(name, func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "bytes")
			c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
			require.NoError(t, err)
			var supervisor *jobs.Supervisor
			if name == "stopped supervisor" {
				supervisor = jobs.New(t.Context(), nil)
				supervisor.Stop()
			}
			cause := s.Start(supervisor, c, root)
			require.Error(t, cause)
			_, err = root.Stat(".")
			require.Error(t, err)
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			require.Equal(t, store.StorageOperationFailed, finished.State)
			require.Equal(t, cause.Error(), finished.Error)
			require.NotNil(t, finished.FinishedAt)
		})
	}
}

func TestCheckoutResumeReconcilesStartedWorkInSupervisor(t *testing.T) {
	t.Parallel()
	for _, replacement := range []string{"missing", "file", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "photo bytes")
			c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
			require.NoError(t, err)
			require.NoError(t, root.Close())
			require.NoError(t, s.Store.ClaimPhotoCheckout(t.Context(), c.ID, op.ID))
			entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.NoError(t, s.Store.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { return true, nil }))
			require.NoError(t, s.Store.ObservePhotoCheckout(t.Context(), c.ID, op.ID, []string{entries[0].FileID}))
			require.NoError(t, os.Remove(c.Destination))
			var keep string
			switch replacement {
			case "file":
				keep = c.Destination
			case "symlink":
				destination := t.TempDir()
				if err := os.Symlink(destination, c.Destination); err != nil {
					t.Skipf("symbolic links unavailable: %v", err)
				}
				keep = filepath.Join(destination, "keep")
			}
			if keep != "" {
				require.NoError(t, os.WriteFile(keep, []byte("keep"), 0o600))
			}
			var begun, ended atomic.Int32
			if replacement == "missing" {
				s.ActivityBegin = func() { begun.Add(1) }
				s.ActivityEnd = func() { ended.Add(1) }
			}
			supervisor := jobs.New(t.Context(), nil)
			defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
			require.NoError(t, s.Resume(t.Context(), supervisor))
			require.Eventually(t, func() bool {
				status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
				return err == nil && status.Checkout.State == "failed"
			}, 5*time.Second, 10*time.Millisecond)
			if replacement == "missing" {
				require.NoError(t, supervisor.Shutdown(context.Background()))
				registered := supervisor.Snapshot()
				require.Len(t, registered, 1)
				require.Equal(t, "storage:"+op.ID, registered[0].Name)
				require.Equal(t, int32(1), begun.Load())
				require.Equal(t, int32(1), ended.Load())
			}
			status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, c.ID, status.Checkout.ID)
			require.Equal(t, op.ID, status.Checkout.OperationID)
			require.Contains(t, status.Checkout.Error, "interrupted")
			if replacement == "file" {
				require.Contains(t, status.Checkout.Error, "reconciliation failed")
			}
			require.Zero(t, status.PublishedFiles)
			require.Zero(t, status.PublishedBytes)
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			require.Zero(t, finished.CompletedObjects)
			require.Zero(t, finished.CopiedObjects)
			require.Zero(t, finished.CopiedBytes)
			observed, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, entries, observed)
			if keep != "" {
				content, err := os.ReadFile(keep)
				require.NoError(t, err)
				require.Equal(t, "keep", string(content))
			}
		})
	}
	t.Run("verified copies and stage cleanup", func(t *testing.T) {
		s := checkoutService(t)
		for _, name := range []string{"one.jpg", "two.jpg", "three.jpg"} {
			checkoutPhoto(t, s, name, name)
		}
		c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()
		require.NoError(t, s.Store.ClaimPhotoCheckout(t.Context(), c.ID, op.ID))
		entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
		require.NoError(t, err)
		require.Len(t, entries, 3)
		for _, e := range entries {
			require.NoError(t, root.MkdirAll(filepath.Dir(filepath.FromSlash(e.Path)), 0o755))
		}
		stream, _, err := s.Blobs.OpenStreamContext(t.Context(), entries[0].Hash)
		require.NoError(t, err)
		bytes, err := io.ReadAll(stream)
		require.NoError(t, err)
		require.NoError(t, stream.Close())
		// A crash after rename can leave verified bytes without a committed observation.
		require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), bytes, 0o600))
		edited := []byte(strings.Repeat("x", int(entries[1].Size)))
		require.NoError(t, root.WriteFile(filepath.FromSlash(entries[1].Path), edited, 0o600))
		require.NoError(t, s.Store.PublishPhotoCheckoutEntry(t.Context(), entries[2], func() (bool, error) { return true, nil }))
		stage, err := filepublish.CreateRootStage(root)
		require.NoError(t, err)
		defer func() { require.NoError(t, stage.Cleanup()) }()
		payload := filepath.Base(stage.File.Name())
		_, err = stage.File.WriteString("partial bytes")
		require.NoError(t, err)
		require.NoError(t, stage.File.Close())
		stage.File = nil

		supervisor := jobs.New(t.Context(), nil)
		defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
		require.NoError(t, s.Resume(t.Context(), supervisor))
		require.Eventually(t, func() bool {
			actual, err := s.Store.PhotoCheckout(t.Context(), c.ID)
			return err == nil && actual.State == "failed"
		}, 5*time.Second, 10*time.Millisecond)
		require.NoError(t, supervisor.Shutdown(context.Background()))
		_, err = root.Stat(payload)
		require.ErrorIs(t, err, os.ErrNotExist)
		observed, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
		require.NoError(t, err)
		require.True(t, observed[0].Published)
		require.False(t, observed[1].Published)
		require.False(t, observed[2].Published)
		actual, err := root.ReadFile(filepath.FromSlash(entries[1].Path))
		require.NoError(t, err)
		require.Equal(t, edited, actual)
		finished, err := s.Store.StorageOperation(t.Context(), op.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), finished.CopiedObjects)
		require.Equal(t, entries[0].Size, finished.CopiedBytes)
		require.Equal(t, store.StorageOperationFailed, finished.State)
	})
}

func TestCheckoutRunOutcomes(t *testing.T) {
	t.Parallel()

	t.Run("resume after deferral preserves frozen bytes", func(t *testing.T) {
		s := checkoutService(t)
		for _, name := range []string{"first.jpg", "second.jpg", "third.jpg"} {
			checkoutPhoto(t, s, name, name)
		}
		ctx := t.Context()
		c, op, root, err := s.Prepare(ctx, store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()
		entries, err := s.Store.PhotoCheckoutEntries(ctx, c.ID)
		require.NoError(t, err)
		asset, err := s.Store.PhotoAssetByID(ctx, entries[0].AssetID)
		require.NoError(t, err)
		node, err := s.Store.NodeByID(ctx, asset.Files[0].NodeID)
		require.NoError(t, err)
		hash, size, err := s.Blobs.Write(strings.NewReader("new bytes"))
		require.NoError(t, err)
		_, _, err = s.Store.ReplaceContent(ctx, node.ID, node.Revision, hash, size, "image/jpeg")
		require.NoError(t, err)
		beforeWork, afterCopy := false, false
		admissionErr := errors.New("synthetic admission failure")
		s.admit = func(ctx context.Context, _ *store.Store, _ string) (bool, error) {
			if !beforeWork {
				beforeWork = true
				return false, admissionErr
			}
			status, err := s.Store.PhotoCheckoutStatus(ctx, c.ID)
			if !assert.NoError(t, err) {
				return false, err
			}
			if status.PublishedFiles == 1 && !afterCopy {
				afterCopy = true
				return false, admissionErr
			}
			return false, nil
		}
		require.ErrorIs(t, s.Run(ctx, c, root), jobs.ErrStorageOperationDeferred)
		started, err := s.Store.PhotoCheckoutStarted(ctx, c.ID)
		require.NoError(t, err)
		require.False(t, started)
		unstarted, err := s.Store.PhotoCheckoutStatus(ctx, c.ID)
		require.NoError(t, err)
		require.Zero(t, unstarted.PublishedFiles)
		files, err := os.ReadDir(c.Destination)
		require.NoError(t, err)
		require.Empty(t, files)
		require.ErrorIs(t, s.Run(ctx, c, root), jobs.ErrStorageOperationDeferred)
		partial, err := s.Store.PhotoCheckoutStatus(ctx, c.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), partial.PublishedFiles)
		observed, err := s.Store.PhotoCheckoutEntries(ctx, c.ID)
		require.NoError(t, err)
		require.True(t, observed[0].Published)
		require.False(t, observed[1].Published)
		require.False(t, observed[2].Published)
		content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
		require.NoError(t, err)
		require.Equal(t, filepath.Base(entries[0].Path), string(content))
		supervisor := jobs.New(ctx, nil)
		defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
		require.NoError(t, s.Start(supervisor, c, root))
		require.Eventually(t, func() bool {
			operation, err := s.Store.StorageOperation(ctx, op.ID)
			return err == nil && operation.FinishedAt != nil
		}, 5*time.Second, 10*time.Millisecond)
		require.NoError(t, supervisor.Shutdown(context.Background()))
		root, err = os.OpenRoot(c.Destination)
		require.NoError(t, err)
		complete, err := s.Store.PhotoCheckoutStatus(ctx, c.ID)
		require.NoError(t, err)
		require.Equal(t, "completed", complete.Checkout.State)
		require.Equal(t, int64(len(entries)), complete.PublishedFiles)
		var copiedBytes int64
		for _, entry := range entries {
			content, err := root.ReadFile(filepath.FromSlash(entry.Path))
			require.NoError(t, err)
			require.Equal(t, filepath.Base(entry.Path), string(content))
			copiedBytes += int64(len(content))
		}
		finished, err := s.Store.StorageOperation(ctx, op.ID)
		require.NoError(t, err)
		require.Equal(t, int64(len(entries)), finished.CopiedObjects)
		require.Equal(t, copiedBytes, finished.CopiedBytes)
	})

	for _, failure := range []string{"operator cancellation", "daemon shutdown", "verification failure", "source excluded"} {
		t.Run(failure, func(t *testing.T) {
			s := checkoutService(t)
			names := []string{"first.jpg", "second.jpg", "third.jpg"}
			for _, name := range names {
				checkoutPhoto(t, s, name, name)
			}
			c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := false
			s.Mutate = func(ctx context.Context, fn func() error) error {
				progress, err := s.Store.PhotoCheckoutStatus(ctx, c.ID)
				require.NoError(t, err)
				if progress.PublishedFiles == 1 && !interrupted && failure == "source excluded" {
					interrupted = true
					asset, err := s.Store.PhotoAssetByID(ctx, entries[1].AssetID)
					require.NoError(t, err)
					_, err = s.Store.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
					require.NoError(t, err)
				}
				err = fn()
				progress, statusErr := s.Store.PhotoCheckoutStatus(ctx, c.ID)
				if !assert.NoError(t, statusErr) {
					return statusErr
				}
				if progress.PublishedFiles != 1 || interrupted {
					return err
				}
				if failure == "verification failure" {
					interrupted = true
					hash, err := packstore.ParseHash(entries[1].Hash)
					require.NoError(t, err)
					layout, err := packstore.NewLayout(filepath.Join(s.VaultRoot, "blobs"), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(layout.LoosePath(hash), []byte(strings.Repeat("x", int(entries[1].Size))), 0o600))
				}
				if failure == "operator cancellation" {
					interrupted = true
					require.NoError(t, s.Store.RequestStorageOperationCancel(ctx, op.ID))
				}
				if failure == "daemon shutdown" {
					interrupted = true
					cancel()
				}
				return err
			}
			runErr := s.Run(ctx, c, root)
			if failure == "operator cancellation" {
				require.NoError(t, runErr)
			} else {
				require.Error(t, runErr)
			}
			switch failure {
			case "verification failure":
				require.True(t, interrupted)
				require.ErrorIs(t, runErr, packstore.ErrContentMismatch)
				_, err := root.Stat(filepath.FromSlash(entries[1].Path))
				require.ErrorIs(t, err, os.ErrNotExist)
			case "daemon shutdown":
				require.ErrorIs(t, runErr, context.Canceled)
			}
			actual, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.True(t, actual[0].Published)
			require.False(t, actual[1].Published)
			content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
			require.NoError(t, err)
			require.Equal(t, filepath.Base(entries[0].Path), string(content))
			status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
			require.NoError(t, err)
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			if failure == "operator cancellation" {
				require.Equal(t, store.StorageOperationCancelled, finished.State)
				require.Equal(t, int64(1), status.PublishedFiles)
				return
			}
			if failure == "daemon shutdown" {
				require.Equal(t, store.StorageOperationRunning, finished.State)
				return
			}
			require.Equal(t, store.StorageOperationFailed, finished.State)
			if failure == "source excluded" {
				require.Contains(t, finished.Error, entries[1].FileID)
				require.Contains(t, finished.Error, entries[1].Path)
				require.True(t, actual[2].Published)
				require.Equal(t, int64(2), status.PublishedFiles)
			} else {
				require.False(t, actual[2].Published)
				require.Equal(t, int64(1), finished.CompletedObjects)
			}
		})
	}
}

func TestCheckoutTerminalReleaseAllowsDestinationReuse(t *testing.T) {
	t.Parallel()
	for _, state := range []store.StorageOperationState{store.StorageOperationFailed, store.StorageOperationCancelled, store.StorageOperationCompleted} {
		t.Run(string(state), func(t *testing.T) {
			s := checkoutService(t)
			asset := checkoutPhoto(t, s, "photo.jpg", "bytes")
			selection := store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}
			destination := t.TempDir()
			checkout, op, root, err := s.Prepare(t.Context(), selection, destination, 0)
			require.NoError(t, err)
			require.NoError(t, root.Close())
			require.NoError(t, s.Store.FinishPhotoCheckout(t.Context(), checkout.ID, op.ID, state, "{}", "", time.Now().Add(time.Hour)))
			_, _, next, err := s.Prepare(t.Context(), selection, destination, 0)
			if state == store.StorageOperationCompleted {
				require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
			} else {
				require.NoError(t, err)
				require.NoError(t, next.Close())
			}
		})
	}
}

func TestCheckoutRecordsInstalledProgressAndAllowsWritesDuringSync(t *testing.T) {
	t.Parallel()
	for _, cancelDuringSync := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelDuringSync), func(t *testing.T) {
			s := checkoutService(t)
			asset := checkoutPhoto(t, s, "photo.jpg", "verified bytes")
			checkout, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), 0)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseSync := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseSync()
			failure := errors.New("synthetic parent directory sync failure")
			s.syncParent = func(*filepublish.RootStage) error { close(started); <-release; return failure }
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx, checkout, root) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("checkout never reached directory sync")
			}
			status, err := s.Store.PhotoCheckoutStatus(t.Context(), checkout.ID)
			require.NoError(t, err)
			require.Equal(t, int64(1), status.PublishedFiles)
			writeCtx, stopWrite := context.WithTimeout(t.Context(), 2*time.Second)
			defer stopWrite()
			_, err = s.Store.MkdirAll(writeCtx, "/other-write")
			require.NoError(t, err)
			if cancelDuringSync {
				cancel()
			}
			releaseSync()
			var runErr error
			select {
			case runErr = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("checkout did not finish")
			}
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			require.Equal(t, int64(1), finished.CompletedObjects)
			require.Equal(t, int64(1), finished.CopiedObjects)
			require.Equal(t, int64(len("verified bytes")), finished.CopiedBytes)
			if cancelDuringSync {
				require.ErrorIs(t, runErr, context.Canceled)
				require.Equal(t, store.StorageOperationRunning, finished.State)
			} else {
				require.ErrorIs(t, runErr, failure)
				require.Equal(t, store.StorageOperationFailed, finished.State)
			}
		})
	}
}

func TestCheckoutPreflightsCollisionsBeforePublishingBytes(t *testing.T) {
	t.Parallel()
	for _, secondName := range []string{"photo.JPG", "PHOTO.jpg"} {
		t.Run(secondName, func(t *testing.T) {
			s := checkoutService(t)
			ctx := t.Context()
			var assets []store.PhotoAsset
			for i, content := range []string{"first", "second"} {
				hash, size, err := s.Blobs.Write(strings.NewReader(content))
				require.NoError(t, err)
				source := t.TempDir()
				run, err := s.Store.BeginIngest(ctx, "photo-import", source)
				require.NoError(t, err)
				name := "photo.JPG"
				if i == 1 {
					name = secondName
				}
				node, err := s.Store.IngestFileExact(ctx, run, s.Store.RootID(), fmt.Sprintf("virtual-%d.jpg", i), hash, size, "image/jpeg", filepath.Join(source, name), "")
				require.NoError(t, err)
				asset, err := s.Store.PhotoAssetForNode(ctx, node.ID)
				require.NoError(t, err)
				assets = append(assets, asset)
			}
			first, second := assets[0], assets[1]
			_, err := s.Store.DetachPhotoFile(ctx, second.ID, second.Revision, second.Files[0].ID, store.PhotoDetachOptions{})
			require.NoError(t, err)
			first, err = s.Store.AttachPhotoFile(ctx, first.ID, first.Revision, second.Files[0].NodeID, store.PhotoRoleImage, nil)
			require.NoError(t, err)
			destination := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(destination, "case-probe"), nil, 0600))
			_, caseErr := os.Stat(filepath.Join(destination, "CASE-PROBE"))
			require.NoError(t, os.Remove(filepath.Join(destination, "case-probe")))
			checkout, op, root, err := s.Prepare(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{first.ID}}, destination, 0)
			require.NoError(t, err)
			defer func() { require.NoError(t, root.Close()) }()
			runErr := s.Run(ctx, checkout, root)
			status, err := s.Store.PhotoCheckoutStatus(ctx, checkout.ID)
			require.NoError(t, err)
			finished, err := s.Store.StorageOperation(ctx, op.ID)
			require.NoError(t, err)
			if secondName == "photo.JPG" || caseErr == nil {
				require.ErrorIs(t, runErr, os.ErrExist)
				require.Equal(t, "failed", status.Checkout.State)
				require.Zero(t, status.PublishedFiles)
				require.Zero(t, finished.CopiedObjects)
				entries, err := s.Store.PhotoCheckoutEntries(ctx, checkout.ID)
				require.NoError(t, err)
				for _, entry := range entries {
					_, err := root.Stat(filepath.FromSlash(entry.Path))
					require.ErrorIs(t, err, os.ErrNotExist)
				}
			} else {
				require.NoError(t, runErr)
				require.Equal(t, "completed", status.Checkout.State)
				require.Equal(t, int64(2), status.PublishedFiles)
			}
		})
	}
}
