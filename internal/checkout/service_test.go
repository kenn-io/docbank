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
	workingDir, err := os.Getwd()
	require.NoError(t, err)
	relative, err := filepath.Rel(workingDir, t.TempDir())
	require.NoError(t, err)
	reject(relative)
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
			cause := s.Start(supervisor, c, root)
			require.Error(t, cause)
			_, err = root.Stat(".")
			require.Error(t, err)
			status, err := s.Store.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, "failed", status.State)
			require.Equal(t, cause.Error(), status.Error)
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
	for _, kind := range []string{"file", "inaccessible"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "inaccessible" && runtime.GOOS == "windows" {
				t.Skip("Unix directory permissions")
			}
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "one.jpg", "one.jpg")
			for _, name := range []string{"two.jpg", "three.jpg"} {
				checkoutPhoto(t, s, name, name)
			}
			bad, badOp, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
			require.NoError(t, err)
			require.NoError(t, root.Close())
			require.NoError(t, s.Store.ClaimPhotoCheckout(t.Context(), bad.ID, badOp.ID))
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

			if kind == "file" {
				require.NoError(t, os.Remove(bad.Destination))
				require.NoError(t, os.WriteFile(bad.Destination, []byte("keep this file"), 0600))
			} else {
				require.NoError(t, os.Chmod(bad.Destination, 0))
				defer func() { require.NoError(t, os.Chmod(bad.Destination, 0700)) }()
				probe, err := os.OpenRoot(bad.Destination)
				if err == nil {
					_ = probe.Close()
					t.Skip("this user can open directories without read permission")
				}
			}
			var begun, ended atomic.Int32
			s.ActivityBegin = func() { begun.Add(1) }
			s.ActivityEnd = func() { ended.Add(1) }
			supervisor := jobs.New(t.Context(), nil)
			defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
			require.NoError(t, s.Resume(t.Context(), supervisor))
			require.Eventually(t, func() bool {
				for _, id := range []string{bad.ID, c.ID} {
					actual, err := s.Store.PhotoCheckout(t.Context(), id)
					if err != nil || actual.State != "failed" {
						return false
					}
				}
				return true
			}, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, supervisor.Shutdown(context.Background()))
			var jobNames []string
			for _, job := range supervisor.Snapshot() {
				jobNames = append(jobNames, job.Name)
			}
			require.ElementsMatch(t, []string{"storage:" + badOp.ID, "storage:" + op.ID}, jobNames)
			require.Equal(t, int32(2), begun.Load())
			require.Equal(t, int32(2), ended.Load())
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
			status, err := s.Store.PhotoCheckout(t.Context(), bad.ID)
			require.NoError(t, err)
			require.Contains(t, status.Error, "interrupted")
			if kind == "file" {
				content, err := os.ReadFile(bad.Destination)
				require.NoError(t, err)
				require.Equal(t, "keep this file", string(content))
			} else {
				require.Contains(t, status.Error, "reconciliation failed")
			}
		})
	}
}

func TestCheckoutInterruptedRunKeepsPartialCopies(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"success", "nonempty destination", "admission retry", "operator cancellation", "daemon shutdown", "destination moved before completion", "collision", "became-invisible"} {
		t.Run(failure, func(t *testing.T) {
			s := checkoutService(t)
			names := []string{"first.jpg", "second.jpg", "third.jpg"}
			if failure == "destination moved before completion" || failure == "success" {
				names = names[:1]
			}
			if failure == "admission retry" {
				names = names[:2]
			}
			var assets []store.PhotoAsset
			for _, name := range names {
				assets = append(assets, checkoutPhoto(t, s, name, name))
			}
			c, op, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			switch failure {
			case "success":
				// The run must copy the frozen base even when the source head changes.
				node, err := s.Store.NodeByID(t.Context(), assets[0].Files[0].NodeID)
				require.NoError(t, err)
				hash, size, err := s.Blobs.Write(strings.NewReader("new bytes"))
				require.NoError(t, err)
				_, _, err = s.Store.ReplaceContent(t.Context(), node.ID, node.Revision, hash, size, "image/jpeg")
				require.NoError(t, err)
				supervisor := jobs.New(t.Context(), nil)
				defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
				require.NoError(t, s.Start(supervisor, c, root))
				require.Eventually(t, func() bool {
					operation, err := s.Store.StorageOperation(t.Context(), op.ID)
					return err == nil && operation.FinishedAt != nil
				}, 5*time.Second, 10*time.Millisecond)
				require.NoError(t, supervisor.Shutdown(context.Background()))
				root, err = os.OpenRoot(c.Destination)
				require.NoError(t, err)
				entries, err = s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
				require.NoError(t, err)
				require.Len(t, entries, 1)
				require.True(t, entries[0].Published)
				content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
				require.NoError(t, err)
				require.Equal(t, "first.jpg", string(content))
				require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), []byte("edited"), 0o600))
				stream, _, err := s.Blobs.OpenStreamContext(t.Context(), entries[0].Hash)
				require.NoError(t, err)
				source, err := io.ReadAll(stream)
				require.NoError(t, err)
				require.NoError(t, stream.Close())
				require.Equal(t, "first.jpg", string(source))
				finished, err := s.Store.StorageOperation(t.Context(), op.ID)
				require.NoError(t, err)
				require.Equal(t, store.StorageOperationCompleted, finished.State)
				require.Equal(t, int64(1), finished.CopiedObjects)
				require.Equal(t, int64(len(content)), finished.CopiedBytes)
				return
			case "nonempty destination":
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
				return
			}
			if failure == "admission retry" {
				inflight := 0
				beforeWork, afterCopy := false, false
				s.ActivityBegin = func() { inflight++ }
				s.ActivityEnd = func() { inflight-- }
				s.Mutate = func(_ context.Context, fn func() error) error { require.Equal(t, 1, inflight); return fn() }
				s.admit = func(ctx context.Context, _ *store.Store, _ string) (bool, error) {
					require.Zero(t, inflight)
					if !beforeWork {
						beforeWork = true
						return false, errors.New("reading storage operation: database is locked")
					}
					status, err := s.Store.PhotoCheckoutStatus(ctx, c.ID)
					require.NoError(t, err)
					if status.PublishedFiles == 1 && !afterCopy {
						afterCopy = true
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
				content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
				require.NoError(t, err)
				require.Equal(t, filepath.Base(entries[0].Path), string(content))
				require.NoError(t, s.Run(t.Context(), c, nil))
				require.Zero(t, inflight)
				complete, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, "completed", complete.Checkout.State)
				require.Equal(t, int64(2), complete.PublishedFiles)
				for _, entry := range entries {
					content, err := root.ReadFile(filepath.FromSlash(entry.Path))
					require.NoError(t, err)
					require.Equal(t, filepath.Base(entry.Path), string(content))
				}
				return
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := false
			s.Mutate = func(ctx context.Context, fn func() error) error {
				progress, err := s.Store.PhotoCheckoutStatus(ctx, c.ID)
				require.NoError(t, err)
				if progress.PublishedFiles == 1 && !interrupted {
					switch failure {
					case "collision":
						interrupted = true
						return os.ErrExist
					case "became-invisible":
						interrupted = true
						return store.ErrPhotoCheckoutSourceUnavailable
					}
				}
				err = fn()
				progress, statusErr := s.Store.PhotoCheckoutStatus(ctx, c.ID)
				require.NoError(t, statusErr)
				if progress.PublishedFiles != 1 || interrupted {
					return err
				}
				if failure == "operator cancellation" {
					interrupted = true
					require.NoError(t, s.Store.RequestStorageOperationCancel(ctx, op.ID))
				}
				if failure == "daemon shutdown" {
					interrupted = true
					cancel()
				}
				if failure == "destination moved before completion" {
					interrupted = true
					moved := filepath.Join(t.TempDir(), "moved")
					if err := os.Rename(c.Destination, moved); err != nil {
						if runtime.GOOS == "windows" {
							t.Skip("Windows pins the opened destination against rename")
						}
						require.NoError(t, err)
					}
					require.NoError(t, os.Mkdir(c.Destination, 0755))
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
			case "collision":
				require.True(t, interrupted)
				require.ErrorIs(t, runErr, os.ErrExist)
			case "daemon shutdown":
				require.ErrorIs(t, runErr, context.Canceled)
			case "destination moved before completion":
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
				require.Equal(t, "running", status.Checkout.State)
				require.Equal(t, store.StorageOperationRunning, finished.State)
				return
			}
			require.Equal(t, "failed", status.Checkout.State)
			switch failure {
			case "collision":
				require.False(t, actual[2].Published)
				require.Equal(t, int64(1), finished.CompletedObjects)
			case "became-invisible":
				require.Contains(t, status.Checkout.Error, entries[1].FileID)
				require.Contains(t, status.Checkout.Error, entries[1].Path)
				require.True(t, actual[2].Published)
				require.Equal(t, int64(2), status.PublishedFiles)
			case "destination moved before completion":
				files, err := os.ReadDir(c.Destination)
				require.NoError(t, err)
				require.Empty(t, files)
			}
		})
	}
}
