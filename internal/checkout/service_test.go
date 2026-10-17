package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	"go.kenn.io/docbank/sqlite"
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

func TestPrepareCancelledWhileWaitingForCreation(t *testing.T) {
	require.NoError(t, creationMu.Acquire(t.Context(), 1))
	defer creationMu.Release(1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	returned := make(chan error, 1)
	go func() {
		_, _, _, _, err := (Service{}).Prepare(ctx, store.PhotoCheckoutSelection{}, "", 0)
		returned <- err
	}()
	select {
	case err := <-returned:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Error("Prepare stayed blocked on the creation lock after cancellation")
	}
}

func TestCheckoutAllowsUnrelatedDestinationAfterReservedAncestorBecomesFile(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	parent := filepath.Join(t.TempDir(), "parent")
	reserved := filepath.Join(parent, "reserved")
	require.NoError(t, os.MkdirAll(reserved, 0o700))
	reservation, _, _, held, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, reserved, 0)
	require.NoError(t, err)
	require.NotEmpty(t, reservation.ID)
	require.NoError(t, held.Close())
	require.NoError(t, os.Remove(reserved))
	require.NoError(t, os.Remove(parent))
	require.NoError(t, os.WriteFile(parent, []byte("replacement"), 0o600))
	destination := t.TempDir()
	checkout, _, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, destination, 0)
	require.NoError(t, err)
	require.NotEmpty(t, checkout.ID)
	require.NoError(t, root.Close())
}

func TestCheckoutRejectsStartupCleanupDirectoryAlias(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		reopenBlobs bool
	}{
		{name: "blobs"},
		{name: filepath.Join("blobs", "tmp")},
		{name: "web-launch"},
		{name: "web-downloads"},
		{name: "blobs", reopenBlobs: true},
	} {
		label := tc.name
		if tc.reopenBlobs {
			label = "relocated primary blobs root"
		}
		t.Run(label, func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "bytes")
			name := tc.name
			managed := filepath.Join(s.VaultRoot, name)
			if tc.reopenBlobs {
				require.NoError(t, s.Blobs.Close())
			}
			if name == "web-launch" || name == "web-downloads" {
				require.NoError(t, os.Mkdir(managed, 0o700))
			}
			alias := filepath.Join(t.TempDir(), "staging")
			require.NoError(t, os.Rename(managed, alias))
			if err := os.Symlink(alias, managed); err != nil {
				t.Skipf("symbolic links unavailable: %v", err)
			}
			if tc.reopenBlobs {
				var err error
				s.Blobs, err = blob.New(store.NewPackCatalog(s.Store), managed)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, s.Blobs.Close()) })
			}
			destination := alias
			if name == "blobs" {
				destination = filepath.Join(alias, "tmp")
			}
			if tc.reopenBlobs {
				destination = filepath.Join(alias, "checkout")
				require.NoError(t, os.Mkdir(destination, 0o700))
			}
			if name == filepath.Join("blobs", "tmp") {
				destination = filepath.Join(alias, "nested", "checkout")
				require.NoError(t, os.MkdirAll(destination, 0o700))
			}
			assertStartupCleanupCheckoutRejected(t, s, a, destination)
		})
	}
}

func assertStartupCleanupCheckoutRejected(t *testing.T, s Service, a store.PhotoAsset, destination string) {
	t.Helper()
	checkout, _, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, destination, 0)
	if root != nil {
		require.NoError(t, root.Close())
	}
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	require.Empty(t, checkout.ID)
	require.ErrorContains(t, err, "destination overlaps a vault, storage root, watched folder, or checkout")
	checkouts, err := s.Store.ListPhotoCheckouts(t.Context())
	require.NoError(t, err)
	require.Empty(t, checkouts)
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
		checkout, _, _, root, err := s.Prepare(t.Context(), selection, destination, 0)
		if root != nil {
			require.NoError(t, root.Close())
		}
		require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
		require.Empty(t, checkout.ID)
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
	destination := t.TempDir()
	c, _, _, root, err := s.Prepare(t.Context(), selection, destination, 0)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	sub := filepath.Join(c.Destination, "nested")
	require.NoError(t, os.Mkdir(sub, 0o755))
	reject(sub)
	missing := filepath.Join(t.TempDir(), "missing")
	s.Config.StoreBindings = map[string]config.StoreBindingConfig{"archive": {Kind: "filesystem", Path: filepath.Join(missing, "archive")}}
	s.Config.Watches = []config.WatchConfig{{Source: filepath.Join(missing, "watch")}}
	c, _, _, root, err = s.Prepare(t.Context(), selection, t.TempDir(), 0)
	require.NoError(t, err)
	require.NotEmpty(t, c.ID)
	require.NoError(t, root.Close())
	t.Run("canonical symlink destination", func(t *testing.T) {
		destination := t.TempDir()
		alias := filepath.Join(t.TempDir(), "checkout")
		if err := os.Symlink(destination, alias); err != nil {
			t.Skipf("symbolic links unavailable: %v", err)
		}
		canonical, err := filepath.EvalSymlinks(alias)
		require.NoError(t, err)
		created, _, _, root, err := s.Prepare(t.Context(), selection, alias, 0)
		require.NoError(t, err)
		defer func() { require.NoError(t, root.Close()) }()
		require.Equal(t, canonical, created.Destination)
	})
	for _, reopen := range []bool{false, true} {
		t.Run(fmt.Sprintf("Run rechecks destination after Prepare reopen=%t", reopen), func(t *testing.T) {
			c, _, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
			require.NoError(t, err)
			defer func() {
				if root != nil {
					require.NoError(t, root.Close())
				}
			}()
			if reopen {
				require.NoError(t, root.Close())
				root = nil
			}
			require.NoError(t, os.WriteFile(filepath.Join(c.Destination, "keep"), []byte("keep"), 0600))
			err = s.Run(t.Context(), c, root)
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
}

func TestCheckoutRetiredCopiesProtectDestinationUntilCleared(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	a := checkoutPhoto(t, s, "photo.jpg", "bytes")
	selection := store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}
	destination := t.TempDir()
	c, _, _, root, err := s.Prepare(t.Context(), selection, destination, 0)
	require.NoError(t, err)
	require.NoError(t, s.Run(t.Context(), c, root))
	require.NoError(t, root.Close())
	retired, err := s.Store.RetirePhotoCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	require.True(t, retired.RetentionReleased)
	status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.PublishedFiles)

	sub := filepath.Join(destination, "nested")
	require.NoError(t, os.Mkdir(sub, 0o700))
	created, _, _, root, err := s.Prepare(t.Context(), selection, sub, 0)
	if root != nil {
		require.NoError(t, root.Close())
	}
	require.ErrorIs(t, err, store.ErrInvalidPhotoCheckout)
	require.Empty(t, created.ID)

	children, err := os.ReadDir(destination)
	require.NoError(t, err)
	for _, child := range children {
		require.NoError(t, os.RemoveAll(filepath.Join(destination, child.Name())))
	}
	created, _, _, root, err = s.Prepare(t.Context(), selection, destination, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	require.NoError(t, s.Run(t.Context(), created, root))
	status, err = s.Store.PhotoCheckoutStatus(t.Context(), created.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.PublishedFiles)
}

func TestCheckoutStartFailurePersistsFailureAndClosesRoot(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"nil supervisor", "stopped supervisor"} {
		t.Run(name, func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "bytes")
			c, op, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
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
	type reconcileCase struct {
		name                string
		setup               string
		state               string
		errorContains       []string
		publishedFiles      int64
		checkPublishedBytes bool
		publishedBytes      bool
		operationProgress   string
		entryPublished      bool
		fileContent         string
		activity            bool
	}
	cases := []reconcileCase{
		{name: "missing", setup: "missing", state: "failed", errorContains: []string{"interrupted"}, operationProgress: "zero", activity: true},
		{name: "file", setup: "file", state: "failed", errorContains: []string{"interrupted", "reconciliation failed"}, operationProgress: "zero", checkPublishedBytes: true, fileContent: "keep"},
		{name: "symlink", setup: "symlink", state: "failed", errorContains: []string{"interrupted"}, operationProgress: "zero", checkPublishedBytes: true, fileContent: "keep"},
		{name: "edited published copy", setup: "published", state: "failed", errorContains: []string{"interrupted"}, publishedFiles: 1, checkPublishedBytes: true, publishedBytes: true, operationProgress: "copied", entryPublished: true, fileContent: "keep"},
		{name: "records an unrecorded installed copy", setup: "unrecorded install", state: "failed", publishedFiles: 1, entryPublished: true, fileContent: "edited working copy"},
		{name: "keeps edited copy without cursor", setup: "no cursor", state: "failed", checkPublishedBytes: true, operationProgress: "zero", fileContent: "edited working copy"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "photo bytes")
			c, op, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, t.TempDir(), 0)
			require.NoError(t, err)
			entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			var keep, payload string
			stagePayload := false
			if test.setup != "unrecorded install" && test.setup != "no cursor" {
				require.NoError(t, s.Store.ClaimPhotoCheckout(t.Context(), c.ID, op.ID))
				require.NoError(t, s.Store.PublishPhotoCheckoutEntry(t.Context(), entries[0], func() (bool, error) { return true, nil }))
				require.NoError(t, s.Store.ObservePhotoCheckout(t.Context(), c.ID, op.ID, []string{entries[0].FileID}))
				require.NoError(t, s.Store.AdvanceStorageOperation(t.Context(), op.ID, entries[0].FileID, 1, 1, entries[0].Size, "{}"))
			}
			switch test.setup {
			case "missing":
				require.NoError(t, root.Close())
				require.NoError(t, os.Remove(c.Destination))
			case "file":
				require.NoError(t, root.Close())
				require.NoError(t, os.Remove(c.Destination))
				keep = c.Destination
				require.NoError(t, os.WriteFile(keep, []byte(test.fileContent), 0o600))
			case "symlink":
				require.NoError(t, root.Close())
				require.NoError(t, os.Remove(c.Destination))
				destination := t.TempDir()
				if err := os.Symlink(destination, c.Destination); err != nil {
					t.Skipf("symbolic links unavailable: %v", err)
				}
				keep = filepath.Join(destination, "keep")
				require.NoError(t, os.WriteFile(keep, []byte(test.fileContent), 0o600))
			case "published":
				keep = filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
				require.NoError(t, os.MkdirAll(filepath.Dir(keep), 0o700))
				require.NoError(t, os.WriteFile(keep, []byte(test.fileContent), 0o600))
				stagePayload = true
			case "unrecorded install", "no cursor":
				require.NoError(t, s.Store.ClaimPhotoCheckout(t.Context(), c.ID, op.ID))
				require.NoError(t, root.MkdirAll(filepath.Dir(filepath.FromSlash(entries[0].Path)), 0o700))
				require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), []byte("photo bytes"), 0o600))
				if test.setup == "unrecorded install" {
					require.NoError(t, s.Store.AdvanceStorageOperation(t.Context(), op.ID, entries[0].FileID, 1, 1, entries[0].Size, "{}"))
				}
				require.NoError(t, root.WriteFile(filepath.FromSlash(entries[0].Path), []byte(test.fileContent), 0o600))
				keep = filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
				stagePayload = true
			}
			if stagePayload {
				stage, err := filepublish.CreateRootStage(root)
				require.NoError(t, err)
				payload = filepath.Join(c.Destination, filepath.Base(stage.File.Name()))
				_, err = stage.File.WriteString("partial bytes")
				require.NoError(t, err)
				require.NoError(t, stage.File.Close())
				require.NoError(t, root.Close())
			}
			var begun, ended atomic.Int32
			if test.activity {
				s.ActivityBegin = func() { begun.Add(1) }
				s.ActivityEnd = func() { ended.Add(1) }
			}
			supervisor := jobs.New(t.Context(), nil)
			defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
			require.NoError(t, s.Resume(t.Context(), supervisor))
			require.Eventually(t, func() bool {
				status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
				return err == nil && status.Checkout.State == test.state
			}, 5*time.Second, 10*time.Millisecond)
			if test.activity {
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
			for _, message := range test.errorContains {
				require.Contains(t, status.Checkout.Error, message)
			}
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			require.Equal(t, test.publishedFiles, status.PublishedFiles)
			if test.checkPublishedBytes {
				want := int64(0)
				if test.publishedBytes {
					want = entries[0].Size
				}
				require.Equal(t, want, status.PublishedBytes)
			}
			switch test.operationProgress {
			case "zero":
				require.Zero(t, finished.CompletedObjects)
				require.Zero(t, finished.CopiedObjects)
				require.Zero(t, finished.CopiedBytes)
			case "copied":
				require.Equal(t, int64(1), finished.CompletedObjects)
				require.Equal(t, int64(1), finished.CopiedObjects)
				require.Equal(t, entries[0].Size, finished.CopiedBytes)
			}
			entries[0].Published = test.entryPublished
			observed, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.Equal(t, entries, observed)
			if keep != "" {
				content, err := os.ReadFile(keep)
				require.NoError(t, err)
				require.Equal(t, test.fileContent, string(content))
			}
			if payload != "" {
				_, err := os.Stat(payload)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func recordCheckoutPublication(t *testing.T, s Service, op store.StorageOperation, entry store.PhotoCheckoutEntry, publication string) {
	t.Helper()
	switch publication {
	case "published":
		require.NoError(t, s.Store.PublishPhotoCheckoutEntry(t.Context(), entry, func() (bool, error) { return true, nil }))
	case "in flight":
		require.NoError(t, s.Store.AdvanceStorageOperation(t.Context(), op.ID, entry.FileID, 0, 0, 0, "{}"))
	}
}

func TestCheckoutRecoveryFinishesReplacedDirectories(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		location    string
		publication string
	}{
		{location: "destination ancestor", publication: "published"},
		{location: "entry subdirectory", publication: "published"},
		{location: "entry subdirectory", publication: "unrecorded"},
	} {
		t.Run(test.location+"/"+test.publication, func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "source bytes")
			parent := filepath.Join(t.TempDir(), "parent")
			destination := filepath.Join(parent, "checkout")
			require.NoError(t, os.MkdirAll(destination, 0o700))
			c, op, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}, destination, 0)
			require.NoError(t, err)
			require.NoError(t, s.Store.ClaimPhotoCheckout(t.Context(), c.ID, op.ID))
			entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			recordCheckoutPublication(t, s, op, entries[0], test.publication)
			require.NoError(t, root.Close())
			replaced := parent
			if test.location == "entry subdirectory" {
				replaced = filepath.Join(destination, filepath.Dir(filepath.FromSlash(entries[0].Path)))
				require.NotEqual(t, destination, replaced)
				require.NoError(t, os.MkdirAll(filepath.Dir(replaced), 0o700))
			} else {
				require.NoError(t, os.RemoveAll(parent))
			}
			require.NoError(t, os.WriteFile(replaced, []byte("user replacement"), 0o600))
			supervisor := jobs.New(t.Context(), nil)
			defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
			require.NoError(t, s.Resume(t.Context(), supervisor))
			require.Eventually(t, func() bool {
				status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID)
				return err == nil && status.Checkout.State == "failed" && status.Checkout.RetentionReleased
			}, 3*time.Second, 10*time.Millisecond)
			finished, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			require.Equal(t, store.StorageOperationFailed, finished.State)
			require.NotNil(t, finished.FinishedAt)
			observed, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			require.False(t, observed[0].Published)
			content, err := os.ReadFile(replaced)
			require.NoError(t, err)
			require.Equal(t, "user replacement", string(content))
		})
	}
}

func TestCheckoutRunOutcomes(t *testing.T) {
	t.Parallel()

	t.Run("resume after deferral preserves frozen bytes", func(t *testing.T) {
		s := checkoutService(t)
		source := t.TempDir()
		run, err := s.Store.BeginIngest(t.Context(), "photo-import", source)
		require.NoError(t, err)
		var members []store.PhotoImportMember
		for _, member := range []struct{ name, role, mediaType string }{
			{"IMG.ARW", store.PhotoRoleRAW, "image/x-sony-arw"},
			{"IMG.JPG", store.PhotoRoleImage, "image/jpeg"},
			{"IMG.ARW.XMP", store.PhotoRoleSidecar, "application/rdf+xml"},
		} {
			hash, size, err := s.Blobs.Write(strings.NewReader(member.name))
			require.NoError(t, err)
			members = append(members, store.PhotoImportMember{Name: member.name, Role: member.role, MediaType: member.mediaType, BlobHash: hash, Size: size, OriginalPath: filepath.Join(source, member.name)})
		}
		group, err := s.Store.IngestPhotoGroup(t.Context(), run, store.PhotoImportGroup{DestinationID: s.Store.RootID(), Members: members})
		require.NoError(t, err)
		ctx := t.Context()
		c, op, _, root, err := s.Prepare(ctx, store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
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
			require.Equal(t, group.Asset.ID, entry.AssetID)
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
	t.Run("publication record failure preserves installed copy", func(t *testing.T) {
		t.Parallel()
		s := checkoutService(t)
		asset := checkoutPhoto(t, s, "photo.jpg", "photo bytes")
		checkout, op, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), 0)
		require.NoError(t, err)
		defer func() { require.NoError(t, root.Close()) }()
		entries, err := s.Store.PhotoCheckoutEntries(t.Context(), checkout.ID)
		require.NoError(t, err)
		db, err := s.Store.SQLiteDriver().Open(filepath.Join(s.VaultRoot, "docbank.db"), sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
		require.NoError(t, err)
		defer func() { require.NoError(t, db.Close()) }()
		_, err = db.ExecContext(t.Context(), `CREATE TRIGGER fail_checkout_publication BEFORE UPDATE OF published ON photo_checkout_entries WHEN NEW.published=1 BEGIN SELECT RAISE(ABORT,'synthetic publication-record failure'); END`)
		require.NoError(t, err)
		err = s.Run(t.Context(), checkout, root)
		require.ErrorIs(t, err, store.ErrPhotoCheckoutPublicationUnrecorded)
		require.ErrorContains(t, err, "synthetic publication-record failure")
		operation, err := s.Store.StorageOperation(t.Context(), op.ID)
		require.NoError(t, err)
		require.Equal(t, store.StorageOperationRunning, operation.State)
		require.Nil(t, operation.FinishedAt)
		require.Equal(t, entries[0].FileID, operation.Cursor)
		status, err := s.Store.PhotoCheckoutStatus(t.Context(), checkout.ID)
		require.NoError(t, err)
		require.Equal(t, "running", status.Checkout.State)
		require.False(t, status.Checkout.RetentionReleased)
		require.Zero(t, status.PublishedFiles)
		content, err := root.ReadFile(filepath.FromSlash(entries[0].Path))
		require.NoError(t, err)
		require.Equal(t, "photo bytes", string(content))
	})
	t.Run("installed progress allows writes during sync", func(t *testing.T) {
		t.Parallel()
		s := checkoutService(t)
		asset := checkoutPhoto(t, s, "photo.jpg", "verified bytes")
		checkout, op, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), 0)
		require.NoError(t, err)
		defer func() { _ = root.Close() }()
		ctx := t.Context()
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
		require.ErrorIs(t, runErr, failure)
		require.Equal(t, store.StorageOperationFailed, finished.State)
	})

	for _, tc := range []struct {
		failure            string
		removeExcludedBlob bool
	}{
		{failure: "operator cancellation"},
		{failure: "daemon shutdown"},
		{failure: "verification failure"},
		{failure: "source excluded", removeExcludedBlob: true},
		{failure: "destination collision"},
	} {
		failure := tc.failure
		t.Run(failure, func(t *testing.T) {
			s := checkoutService(t)
			names := []string{"first.jpg", "second.jpg", "third.jpg"}
			for _, name := range names {
				checkoutPhoto(t, s, name, name)
			}
			c, op, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{All: true}, t.TempDir(), 100)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			entries, err := s.Store.PhotoCheckoutEntries(t.Context(), c.ID)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := false
			s.Mutate = func(ctx context.Context, fn func() error) error {
				err := fn()
				progress, statusErr := s.Store.PhotoCheckoutStatus(ctx, c.ID)
				if !assert.NoError(t, statusErr) {
					return statusErr
				}
				if progress.PublishedFiles != 1 || interrupted {
					return err
				}
				if failure == "source excluded" {
					interrupted = true
					asset, err := s.Store.PhotoAssetByID(ctx, entries[1].AssetID)
					require.NoError(t, err)
					_, err = s.Store.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
					require.NoError(t, err)
					if tc.removeExcludedBlob {
						require.NoError(t, s.Blobs.Remove(entries[1].Hash))
						_, _, err = s.Blobs.OpenStreamContext(ctx, entries[1].Hash)
						require.Error(t, err)
					}
				}
				if failure == "verification failure" {
					interrupted = true
					hash, err := packstore.ParseHash(entries[1].Hash)
					require.NoError(t, err)
					layout, err := packstore.NewLayout(filepath.Join(s.VaultRoot, "blobs"), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(layout.LoosePath(hash), []byte(strings.Repeat("x", int(entries[1].Size))), 0o600))
				}
				if failure == "destination collision" {
					interrupted = true
					path := filepath.FromSlash(entries[1].Path)
					require.NoError(t, root.MkdirAll(filepath.Dir(path), 0o700))
					require.NoError(t, root.WriteFile(path, []byte("user copy"), 0o600))
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
			case "source excluded":
				require.ErrorIs(t, runErr, store.ErrPhotoCheckoutSourceUnavailable)
			case "verification failure":
				require.True(t, interrupted)
				require.ErrorIs(t, runErr, packstore.ErrContentMismatch)
				_, err := root.Stat(filepath.FromSlash(entries[1].Path))
				require.ErrorIs(t, err, os.ErrNotExist)
			case "destination collision":
				require.ErrorContains(t, runErr, entries[1].Path)
				require.ErrorContains(t, runErr, entries[1].FileID)
				content, err := root.ReadFile(filepath.FromSlash(entries[1].Path))
				require.NoError(t, err)
				require.Equal(t, "user copy", string(content))
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
				content, err := root.ReadFile(filepath.FromSlash(entries[2].Path))
				require.NoError(t, err)
				require.Equal(t, filepath.Base(entries[2].Path), string(content))
				require.Equal(t, int64(2), status.PublishedFiles)
			} else {
				require.False(t, actual[2].Published)
				require.Equal(t, int64(1), finished.CompletedObjects)
			}
		})
	}
}

func TestCheckoutRechecksProtectedRootSnapshot(t *testing.T) {
	t.Parallel()
	for _, state := range []store.StorageOperationState{"created", store.StorageOperationFailed, store.StorageOperationCancelled} {
		t.Run(string(state), func(t *testing.T) {
			s := checkoutService(t)
			a := checkoutPhoto(t, s, "photo.jpg", "source")
			selection := store.PhotoCheckoutSelection{AssetIDs: []string{a.ID}}
			destination := t.TempDir()
			var other store.PhotoCheckout
			var op store.StorageOperation
			if state != "created" {
				var err error
				other, op, _, err = s.Store.CreatePhotoCheckout(t.Context(), selection, destination, 0)
				require.NoError(t, err)
			}
			s.Mutate = func(ctx context.Context, fn func() error) error {
				var err error
				if state == "created" {
					_, _, _, err = s.Store.CreatePhotoCheckout(ctx, selection, destination, 0)
				} else {
					err = s.Store.FinishPhotoCheckout(ctx, other.ID, op.ID, state, "{}", "", time.Now().Add(time.Hour))
				}
				if err != nil {
					return err
				}
				return fn()
			}
			_, _, _, root, err := s.Prepare(t.Context(), selection, t.TempDir(), 0)
			require.ErrorContains(t, err, "protected roots changed")
			require.ErrorIs(t, err, store.ErrPhotoCheckoutConflict)
			require.Nil(t, root)
		})
	}
}

func TestCheckoutSeparationAvoidsRepeatedAndObjectTreeWalks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix directory permissions")
	}
	for _, tree := range []string{"vault", "blobs", "watch"} {
		t.Run(tree, func(t *testing.T) {
			s := checkoutService(t)
			asset := checkoutPhoto(t, s, "photo.jpg", "source")
			protected := s.VaultRoot
			if tree == "blobs" {
				protected = filepath.Join(s.VaultRoot, "blobs")
			}
			if tree == "watch" {
				protected = t.TempDir()
				s.Config.Watches = []config.WatchConfig{{Source: protected}}
			}
			child := filepath.Join(protected, "unreadable")
			require.NoError(t, os.Mkdir(child, 0o700))
			t.Cleanup(func() { require.NoError(t, os.Chmod(child, 0o700)) })
			if tree != "watch" {
				require.NoError(t, os.Chmod(child, 0o000))
			}
			checkout, _, _, root, err := s.Prepare(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, t.TempDir(), 0)
			require.NoError(t, err)
			defer func() { require.NoError(t, root.Close()) }()
			require.NoError(t, os.Chmod(child, 0o000))
			require.NoError(t, s.Run(t.Context(), checkout, root))
			status, err := s.Store.PhotoCheckoutStatus(t.Context(), checkout.ID)
			require.NoError(t, err)
			require.EqualValues(t, 1, status.PublishedFiles)
			require.Equal(t, "completed", status.Checkout.State)
		})
	}
}
