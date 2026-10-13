package checkout

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
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

func TestCheckoutCopiesWritableBases(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "original bytes")
	destination := t.TempDir()
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, destination, 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	// The run must copy the frozen base even when the source head changes.
	node, err := s.Store.NodeByID(t.Context(), a.Files[0].NodeID)
	require.NoError(t, err)
	hash, size, err := s.Blobs.Write(strings.NewReader("new bytes"))
	require.NoError(t, err)
	_, _, err = s.Store.ReplaceContent(t.Context(), node.ID, node.Revision, hash, size, "image/jpeg")
	require.NoError(t, err)
	require.NoError(t, s.Run(t.Context(), c, root))
	entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Published)
	content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
	require.NoError(t, err)
	require.Equal(t, "original bytes", string(content))
	require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), []byte("edited"), 0o600))
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationCompleted, finished.State)
	require.Equal(t, int64(1), finished.CopiedObjects)
	require.Equal(t, int64(len(content)), finished.CopiedBytes)
	status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
}

func TestCheckoutCancellation(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	require.NoError(t, s.Store.RequestStorageOperationCancel(t.Context(), op.ID))
	require.NoError(t, s.Run(t.Context(), c, root))
	finished, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, store.StorageOperationCancelled, finished.State)
	entries, err := os.ReadDir(c.Destination)
	require.NoError(t, err)
	require.Empty(t, entries)
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
	prior := t.TempDir()
	reserved, _, priorRoot, err := s.Prepare(t.Context(), selection, prior, 0)
	require.NoError(t, err)
	require.NoError(t, priorRoot.Close())
	require.NoError(t, os.Remove(reserved.Destination))
	require.NoError(t, os.WriteFile(reserved.Destination, []byte("keep"), 0600))
	_, _, unrelatedRoot, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, unrelatedRoot.Close())
	missing := filepath.Join(t.TempDir(), "missing")
	s.Config.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: filepath.Join(missing, "archive")}}
	s.Config.Watches = []config.WatchConfig{{Source: filepath.Join(missing, "watch")}}
	_, _, root, err = s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
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
			require.Error(t, s.Start(supervisor, c, root))
			_, err = root.Stat(".")
			require.Error(t, err)
			status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, "failed", status.State)
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			require.Equal(t, store.StorageOperationFailed, finished.State)
			require.Equal(t, finished.Error, status.Error)
			require.NotNil(t, finished.FinishedAt)
		})
	}
}

func TestCheckoutRecoveryContinuesAfterDestinationBecomesUnusable(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"file", "inaccessible"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "inaccessible" && runtime.GOOS == "windows" {
				t.Skip("Unix directory permissions")
			}
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "bytes")
			c, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
			require.NoError(t, err)
			require.NoError(t, root.Close())
			if kind == "file" {
				require.NoError(t, os.Remove(c.Destination))
				require.NoError(t, os.WriteFile(c.Destination, []byte("keep this file"), 0600))
			} else {
				require.NoError(t, os.Chmod(c.Destination, 0))
				defer func() { require.NoError(t, os.Chmod(c.Destination, 0700)) }()
				probe, err := os.OpenRoot(c.Destination)
				if err == nil {
					_ = probe.Close()
					t.Skip("this user can open directories without read permission")
				}
			}
			require.NoError(t, s.reconcile(t.Context(), c))
			status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, "failed", status.State)
			require.Contains(t, status.Error, "interrupted")
			if kind == "file" {
				content, err := os.ReadFile(c.Destination)
				require.NoError(t, err)
				require.Equal(t, "keep this file", string(content))
			} else {
				require.Contains(t, status.Error, "reconciliation failed")
			}
		})
	}
}

func TestCheckoutRecoveryRebuildsCopyObservationsAfterPublicationCrash(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	for _, name := range []string{"one.jpg", "two.jpg", "three.jpg"} {
		checkoutPhoto(t, s, name, name)
	}
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
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
	require.NoError(t, s.reconcile(t.Context(), c))
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
}

func TestCheckoutResumeReconcilesStartedWorkInSupervisor(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	require.NoError(t, s.Store.ClaimPhotoCheckout(t.Context(), c.ID, op.ID))
	var begun, ended atomic.Int32
	s.ActivityBegin = func() { begun.Add(1) }
	s.ActivityEnd = func() { ended.Add(1) }
	supervisor := jobs.New(t.Context(), nil)
	defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
	require.NoError(t, s.Resume(t.Context(), supervisor))
	require.Eventually(t, func() bool {
		actual, err := s.Store.PhotoCheckout(t.Context(), c.ID)
		return err == nil && actual.State == "failed" && strings.Contains(actual.Error, "interrupted")
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, supervisor.Shutdown(context.Background()))
	require.Equal(t, int32(1), begun.Load())
	require.Equal(t, int32(1), ended.Load())
}

func TestCheckoutFailedRunKeepsPartialCopies(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"cancel", "destination moved after first copy", "destination moved before completion", "collision", "became-invisible"} {
		t.Run(failure, func(t *testing.T) {
			s := checkoutService(t)
			names := []string{"first.jpg", "second.jpg", "third.jpg"}
			if failure == "destination moved before completion" {
				names = names[:1]
			}
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
			publications := 0
			if failure == "collision" {
				collision := filepath.FromSlash(entries[1].Path)
				require.NoError(t, root.MkdirAll(filepath.Dir(collision), 0755))
				require.NoError(t, root.WriteFile(collision, []byte("keep my edit"), 0600))
			}
			s.Mutate = func(ctx context.Context, fn func() error) error {
				publications++
				if failure == "became-invisible" && publications == 2 {
					blocked, err := s.Store.PhotoAssetByID(ctx, entries[1].AssetID)
					require.NoError(t, err)
					_, err = s.Store.TrashPhotoAsset(ctx, blocked.ID, blocked.Revision)
					require.NoError(t, err)
				}
				err := fn()
				if publications == 1 {
					switch failure {
					case "cancel":
						cancel()
					case "destination moved after first copy", "destination moved before completion":
						moved := filepath.Join(t.TempDir(), "moved")
						if err := os.Rename(c.Destination, moved); err != nil {
							if runtime.GOOS == "windows" {
								t.Skip("Windows pins the opened destination against rename")
							}
							require.NoError(t, err)
						}
						require.NoError(t, os.Mkdir(c.Destination, 0755))
					}
				}
				return err
			}
			runErr := s.Run(ctx, c, root)
			require.Error(t, runErr)
			if failure == "cancel" {
				require.ErrorIs(t, runErr, context.Canceled)
			} else if failure != "collision" && failure != "became-invisible" {
				require.ErrorIs(t, runErr, store.ErrInvalidPhotoCheckout)
			}
			actual, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.True(t, actual[0].Published)
			if len(actual) > 1 {
				require.False(t, actual[1].Published)
			}
			content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
			require.NoError(t, err)
			require.NotEmpty(t, content)
			status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
			require.NoError(t, err)
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			if failure == "cancel" {
				require.Equal(t, "running", status.Checkout.State)
				require.Equal(t, store.StorageOperationRunning, finished.State)
				return
			}
			require.Equal(t, "failed", status.Checkout.State)
			switch failure {
			case "collision":
				require.False(t, actual[2].Published)
				content, err := root.ReadFile(filepath.FromSlash(entries[1].Path))
				require.NoError(t, err)
				require.Equal(t, "keep my edit", string(content))
				require.Equal(t, int64(1), finished.CompletedObjects)
			case "became-invisible":
				require.Contains(t, status.Checkout.Error, entries[1].FileID)
				require.Contains(t, status.Checkout.Error, entries[1].Path)
				require.True(t, actual[2].Published)
				require.Equal(t, 3, publications)
			case "destination moved after first copy", "destination moved before completion":
				files, err := os.ReadDir(c.Destination)
				require.NoError(t, err)
				require.Empty(t, files)
			}
		})
	}
}

func TestCheckoutQueuedDestinationRevalidatedBeforeCopy(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}
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
}

func TestCheckoutAdmissionRetryKeepsPartialCopies(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	checkoutPhoto(t, s, "one.jpg", "one")
	checkoutPhoto(t, s, "two.jpg", "two")
	c, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	inflight, admissions := 0, 0
	s.ActivityBegin = func() { inflight++ }
	s.ActivityEnd = func() { inflight-- }
	s.Mutate = func(_ context.Context, fn func() error) error { require.Equal(t, 1, inflight); return fn() }
	s.admit = func(context.Context, *store.Store, string) (bool, error) {
		admissions++
		require.Zero(t, inflight)
		if admissions == 1 || admissions == 4 {
			return false, errors.New("reading storage operation: database is locked")
		}
		return false, nil
	}
	require.ErrorIs(t, s.Run(t.Context(), c, nil), jobs.ErrStorageOperationDeferred)
	require.Zero(t, inflight)
	started, err := s.Store.PhotoCheckoutStarted(t.Context(), c.ID)
	require.NoError(t, err)
	require.False(t, started)
	unstarted, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, "queued", unstarted.Checkout.State)
	require.Zero(t, unstarted.PublishedFiles)
	files, err := os.ReadDir(c.Destination)
	require.NoError(t, err)
	require.Empty(t, files)
	require.ErrorIs(t, s.Run(t.Context(), c, nil), jobs.ErrStorageOperationDeferred)
	require.Zero(t, inflight)
	partial, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), partial.PublishedFiles)
	require.NoError(t, s.Run(t.Context(), c, nil))
	require.Zero(t, inflight)
	complete, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), complete.PublishedFiles)
}
