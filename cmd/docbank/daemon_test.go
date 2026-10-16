package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/color"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/packstore"

	docbank "go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func TestDerivedBackfillsAreRegisteredOnce(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.JPEG(32, 24, color.White)))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "synthetic-photo.jpg", written.Hash, written.Size, "image/jpeg",
		store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize})
	require.NoError(t, err)
	logger := slog.New(slog.DiscardHandler)
	supervisor := jobs.New(t.Context(), logger)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, supervisor.Shutdown(ctx))
	})

	require.NoError(t, startProcessingJobs(
		supervisor, catalog, blobs, t.TempDir(), processing.NewRenditionRuntimeRegistry(), 1,
		api.NewOperationGate(), logger,
	))
	registered := map[string]int{}
	for _, job := range supervisor.Snapshot() {
		if job.Name == "derive:document-events" || job.Name == "derive:document-people" || job.Name == "derive:photo-quality" || job.Name == "extract:photo-sidecars" {
			registered[job.Name]++
		}
	}
	require.Equal(t, map[string]int{"derive:document-events": 1, "derive:document-people": 1, "derive:photo-quality": 1, "extract:photo-sidecars": 1}, registered)
	emptyQuery, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	var signals *document.PhotoQualitySignals
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		page, err := catalog.ListPhotoAssets(t.Context(), store.PhotoBrowseRequest{Query: emptyQuery}, nil)
		if assert.NoError(c, err) && assert.Len(c, page.Items, 1) {
			assert.Equal(c, node.CurrentVersionID, page.Items[0].ContentVersionID)
			signals = page.Items[0].Quality
			assert.NotNil(c, signals)
		}
	}, daemonStartTimeout, 50*time.Millisecond)
	require.NoError(t, document.ValidatePhotoQualitySignals(*signals))
	assert.InDelta(t, 1, signals.Brightness, 0.01)
	assert.InDelta(t, 0, signals.Focus, 0.01)
	assert.InDelta(t, 1, signals.Blur, 0.01)
}

func TestWebOriginUsesDedicatedEphemeralLoopbackListeners(t *testing.T) {
	first, firstURL, err := listenWebOriginWithIdentity(
		t.Context(), "00000000000000000000000000000000")
	require.NoError(t, err)
	require.NoError(t, first.Close())
	second, secondURL, err := listenWebOriginWithIdentity(
		t.Context(), "01010101010101010101010101010101")
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	assert.NotEqual(t, firstURL, secondURL)
	for _, test := range []struct {
		listener net.Listener
		rawURL   string
		host     string
	}{
		{first, firstURL, "docbank-00000000000000000000000000000000.localhost"},
		{second, secondURL, "docbank-01010101010101010101010101010101.localhost"},
	} {
		u, err := url.Parse(test.rawURL)
		require.NoError(t, err)
		assert.Equal(t, test.host, u.Hostname())
		assert.Equal(t, "/", u.Path)
		assert.Empty(t, u.RawQuery)
		assert.Empty(t, u.Fragment)

		listener := test.listener
		host, port, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", host)
		assert.NotEqual(t, "0", port)
		assert.Equal(t, port, u.Port())
	}

	disabled, disabledURL, err := listenWebOrigin(t.Context(), false)
	require.NoError(t, err)
	assert.Nil(t, disabled)
	assert.Empty(t, disabledURL)
}

func TestServeLocksBeforeInitializingVault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "restore-target")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	lock, err := (home.Layout{Root: dir}).TryLockExclusive()
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })
	t.Setenv("DOCBANK_HOME", dir)

	err = runServe(context.Background())
	require.ErrorIs(t, err, home.ErrVaultLocked)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "vault.lock", entries[0].Name(),
		"daemon startup must not initialize a restore-owned target")
}

func TestServeRejectsOwnedProcessingSpoolBeforeCleanup(t *testing.T) {
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, os.MkdirAll(layout.BlobTmpDir(), 0o700))
	embedded, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{SpoolDirectory: layout.BlobTmpDir()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, embedded.Close()) })
	active := filepath.Join(layout.BlobTmpDir(), ".docbank-upload-active")
	require.NoError(t, os.Mkdir(active, 0o700))
	source := filepath.Join(active, "source")
	require.NoError(t, os.WriteFile(source, []byte("active upload"), 0o600))
	t.Setenv("DOCBANK_HOME", layout.Root)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.ErrorIs(t, runServe(ctx), docbank.ErrProcessingSpoolLocked)
	content, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, "active upload", string(content))
}

func TestServeRecoversInterruptedRestoreBeforeInitializingVault(t *testing.T) {
	dir := t.TempDir()
	digest := ""
	handoff, err := blob.NewPrimaryRestoreHandoff(
		filepath.Join(dir, "blobs"),
		packstore.Ownership{
			Format: packstore.OwnershipFormatV1,
			Vault:  "50000000-0000-4000-8000-000000000001",
			Store:  "50000000-0000-4000-8000-000000000002",
			Epoch:  "50000000-0000-4000-8000-000000000003",
		},
		&digest,
	)
	require.NoError(t, err)
	require.NoError(t, handoff.Prepare(t.Context()))
	t.Setenv("DOCBANK_HOME", dir)

	startServe(t)
	waitForDaemon(t, dir)
	pending, err := blob.PrimaryRestoreHandoffPending(filepath.Join(dir, "blobs"))
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestConfiguredWatchStoresRejectOverlapBeforeDaemonStartup(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Watches = []config.WatchConfig{{
		Name: "inbox", Source: root, Destination: "/inbox",
	}}
	cfg.StoreBindings = map[string]config.StoreBindingConfig{
		"archive": {Kind: "filesystem", Path: root},
	}
	stores := []store.BlobStore{{
		ID:   "10000000-0000-4000-8000-000000000001",
		Name: "archive", Kind: "filesystem", Role: "secondary",
		Lifecycle: "active", Binding: "archive",
	}}

	err := validateConfiguredWatchStores(cfg, stores)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlaps filesystem store")
}

func TestServeLocksExistingAncestorBeforeCreatingVault(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "restore-target")
	require.NoError(t, os.Mkdir(parent, 0o700))
	lock, err := (home.Layout{Root: parent}).TryLockExclusive()
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })

	nested := filepath.Join(parent, "docbank.db")
	t.Setenv("DOCBANK_HOME", nested)
	err = runServe(context.Background())
	require.ErrorIs(t, err, home.ErrVaultLocked)
	_, err = os.Lstat(nested)
	require.ErrorIs(t, err, os.ErrNotExist,
		"daemon startup must not create a nested root beneath a restore-owned target")
}

func TestServeServesAndShutsDownGracefully(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)

	stop := startServe(t)
	// Discover via the runtime record like a real client would.
	waitForDaemon(t, dir)

	// Second daemon on the same vault must refuse.
	err := runServe(context.Background())
	require.Error(t, err)

	embeddedConfig := docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{SpoolDirectory: filepath.Join(dir, "blobs", "tmp")}}
	embedded, err := docbank.New(t.Context(), embeddedConfig)
	if embedded != nil {
		t.Cleanup(func() { require.NoError(t, embedded.Close()) })
	}
	require.ErrorIs(t, err, docbank.ErrProcessingSpoolLocked)

	stop()
	// Record removed on shutdown.
	recs, err := daemonconn.RuntimeStore(dir).List()
	require.NoError(t, err)
	assert.Empty(t, recs)
	embedded, err = docbank.New(t.Context(), embeddedConfig)
	require.NoError(t, err)
	require.NoError(t, embedded.Close())
}

// TestServeRequiresKeyEvenWhenConfigIsKeyless is the regression test for the
// keyless-loopback finding: an unconfigured api_key must not mean
// "unauthenticated," even though it's still valid to leave api_key unset on
// a loopback bind. The daemon must generate an ephemeral key and require it
// on every authenticated route regardless of who's asking.
func TestServeRequiresKeyEvenWhenConfigIsKeyless(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)

	startServe(t)
	rec := waitForDaemon(t, dir)

	// No X-Api-Key at all: any local OS user reaching the loopback port
	// without a key must be refused, not silently served.
	resp, err := http.Get("http://" + rec.Address + "/api/v1/nodes/1")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// The daemon still generated a per-run key and published it: the same-
	// user CLI path (runtime record) must be able to use it successfully.
	key := rec.Metadata["api_key"]
	require.NotEmpty(t, key, "runtime record must carry the ephemeral api key")
	req, err := http.NewRequest(http.MethodGet, "http://"+rec.Address+"/api/v1/nodes/1", nil)
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", key)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
}

// Fresh SQLite initialization and shutdown can each exceed ten seconds on busy
// Windows CI runners. These tests check daemon behavior, not performance.
const (
	daemonStartTimeout    = time.Minute
	daemonShutdownTimeout = 30 * time.Second
)

// startServe runs the daemon until stop or test cleanup, whichever comes first,
// and waits for it to exit so TempDir removal never races a live daemon.
func startServe(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()
	stop = sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(daemonShutdownTimeout):
			require.Fail(t, "daemon did not shut down")
		}
	})
	t.Cleanup(stop)
	return stop
}

// waitForDaemon discovers the daemon through its runtime record like a real
// client and waits until it serves health checks, since discovery is published
// before worker registration finishes.
func waitForDaemon(t *testing.T, dir string) kitdaemon.RuntimeRecord {
	t.Helper()
	healthClient := &http.Client{Timeout: time.Second}
	var rec kitdaemon.RuntimeRecord
	require.Eventually(t, func() bool {
		recs, err := daemonconn.RuntimeStore(dir).List()
		if err != nil || len(recs) != 1 {
			return false
		}
		rec = recs[0]
		resp, err := healthClient.Get("http://" + rec.Address + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, daemonStartTimeout, 50*time.Millisecond)
	return rec
}

func newCheckoutReservationFixture(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(layout.DBPath())
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	destination := t.TempDir()
	checkout, operation, _, err := catalog.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, 0)
	require.NoError(t, err)
	return catalog, checkout.ID, operation.ID, destination
}

func TestConfiguredCheckoutValidationDefersOnlyOverlappingBindings(t *testing.T) {
	t.Parallel()
	catalog, checkoutID, operationID, destination := newCheckoutReservationFixture(t)
	defer func() { require.NoError(t, catalog.Close()) }()
	require.NoError(t, catalog.FinishPhotoCheckout(t.Context(), checkoutID, operationID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	_, err := catalog.RetirePhotoCheckout(t.Context(), checkoutID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(destination, "edited.jpg"), []byte("copy"), 0o600))
	admit := newCheckoutBindingAdmission(catalog, slog.New(slog.DiscardHandler))
	cfg := config.Default()
	cfg.StoreBindings = map[string]config.StoreBindingConfig{
		"archive": {Kind: "filesystem", Path: destination},
		"other":   {Kind: "filesystem", Path: t.TempDir()},
		"missing": {Kind: "filesystem", Path: filepath.Join(destination, "missing")},
	}
	stores := []store.BlobStore{
		{Name: "other", Kind: "filesystem", Role: "secondary", Lifecycle: "active", Binding: "other"},
		{Name: "missing", Kind: "filesystem", Role: "secondary", Lifecycle: "active", Binding: "missing"},
	}
	// A watch over the copies waits in its own watcher, not at startup.
	cfg.Watches = []config.WatchConfig{{Name: "checkout", Source: destination}}
	pending, err := validateConfiguredCheckoutStartup(t.Context(), cfg, stores, admit)
	require.NoError(t, err)
	require.False(t, pending)

	stores = append(stores, store.BlobStore{Name: "archive", Kind: "filesystem", Role: "secondary", Lifecycle: "active", Binding: "archive"})
	pending, err = validateConfiguredCheckoutStartup(t.Context(), cfg, stores, admit)
	require.NoError(t, err)
	require.True(t, pending)
	err = admit(t.Context(), cfg.StoreBindings["archive"])
	require.ErrorIs(t, err, errCheckoutPathOverlap)
	require.ErrorContains(t, err, checkoutID)
	require.ErrorContains(t, err, fmt.Sprintf("%q", destination))
}

func TestConfiguredCheckoutStartupAdmitsModerateBinding(t *testing.T) {
	catalog, _, _, _ := newCheckoutReservationFixture(t)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	cfg := config.Default()
	binding := config.StoreBindingConfig{Kind: "filesystem", Path: t.TempDir()}
	cfg.StoreBindings = map[string]config.StoreBindingConfig{"archive": binding}
	admission := &checkoutBindingAdmission{catalog: catalog, logger: slog.New(slog.DiscardHandler),
		check: func(ctx context.Context, _ []store.PhotoCheckout, _ string) error {
			select {
			case <-time.After(300 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	pending, err := validateConfiguredCheckoutStartup(t.Context(), cfg, []store.BlobStore{
		{Kind: "filesystem", Role: "secondary", Lifecycle: "active", Binding: "archive"},
	}, admission.admit)
	require.NoError(t, err)
	require.False(t, pending, "a 300ms separation walk should finish during startup")
	_, admitted := admission.checked[binding]
	require.True(t, admitted)
}

func TestConfiguredCheckoutStartupDefersSlowBinding(t *testing.T) {
	t.Parallel()
	catalog, _, _, _ := newCheckoutReservationFixture(t)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	cfg := config.Default()
	cfg.StoreBindings = map[string]config.StoreBindingConfig{
		"slow": {Kind: "filesystem", Path: t.TempDir()},
		"fast": {Kind: "filesystem", Path: t.TempDir()},
	}
	var fastChecked bool
	admission := &checkoutBindingAdmission{catalog: catalog, logger: slog.New(slog.DiscardHandler),
		check: func(ctx context.Context, _ []store.PhotoCheckout, path string) error {
			if path == cfg.StoreBindings["slow"].Path {
				<-ctx.Done()
				return ctx.Err()
			}
			fastChecked = true
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*blob.RegistryProbeTimeout)
	defer cancel()
	pending, err := validateConfiguredCheckoutStartup(ctx, cfg, []store.BlobStore{
		{Kind: "filesystem", Role: "secondary", Lifecycle: "active", Binding: "slow"},
		{Kind: "filesystem", Role: "secondary", Lifecycle: "active", Binding: "fast"},
	}, admission.admit)
	require.NoError(t, err)
	require.NoError(t, ctx.Err())
	require.True(t, pending)
	require.True(t, fastChecked)
	_, admitted := admission.checked[cfg.StoreBindings["slow"]]
	require.False(t, admitted)
	_, admitted = admission.checked[cfg.StoreBindings["fast"]]
	require.True(t, admitted)
}

func TestCheckoutValidationDefersInternalDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		err     error
		parent  error
		pending bool
	}{
		{name: "traversal deadline", err: fmt.Errorf("traversing checkout: %w", context.DeadlineExceeded), pending: true},
		{name: "unavailable directory", err: &os.PathError{Op: "open", Path: "checkout", Err: os.ErrPermission}, pending: true},
		{name: "parent cancelled", err: context.DeadlineExceeded, parent: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if errors.Is(tc.parent, context.Canceled) {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			require.Equal(t, tc.pending, checkoutValidationPending(ctx, tc.err))
		})
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		checked, admitted := make(chan struct{}, 1), make(chan error, 1)
		var readable atomic.Bool
		go func() {
			admitted <- waitForCheckoutValidation(ctx, func(context.Context) error {
				if readable.Load() {
					return nil
				}
				select {
				case checked <- struct{}{}:
				default:
				}
				return context.DeadlineExceeded
			}, slog.Default())
		}()
		<-checked
		select {
		case err := <-admitted:
			t.Fatalf("admission finished before validation succeeded: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		readable.Store(true)
		require.NoError(t, <-admitted)
	})
}

func TestCheckoutBindingAdmissionCachesSuccessfulSeparation(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix directory permissions")
	}
	catalog, checkoutID, operationID, destination := newCheckoutReservationFixture(t)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	var log bytes.Buffer
	admit := newCheckoutBindingAdmission(catalog, slog.New(slog.NewTextHandler(&log, nil)))
	binding := config.StoreBindingConfig{Kind: "filesystem", Path: t.TempDir()}
	child := filepath.Join(binding.Path, "unreadable")
	require.NoError(t, os.Mkdir(child, 0o700))
	t.Cleanup(func() { require.NoError(t, os.Chmod(child, 0o700)) })
	require.NoError(t, admit(t.Context(), binding))
	require.NoError(t, os.Chmod(child, 0o000))
	// A refresh uses the previous separation result, without opening descendants.
	require.NoError(t, admit(t.Context(), binding))
	changedBinding := binding
	changedBinding.Priority++
	require.ErrorIs(t, admit(t.Context(), changedBinding), os.ErrPermission)
	// A checkout change invalidates the cached result for the original binding.
	require.NoError(t, catalog.FinishPhotoCheckout(t.Context(), checkoutID, operationID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	_, err := catalog.RetirePhotoCheckout(t.Context(), checkoutID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(destination, "copy.jpg"), []byte("copy"), 0o600))
	require.ErrorIs(t, admit(t.Context(), binding), os.ErrPermission)
	require.NoError(t, os.Chmod(child, 0o700))
	require.NoError(t, admit(t.Context(), binding), "failed checks must retry")
	binding.Path = destination
	require.ErrorIs(t, admit(t.Context(), binding), errCheckoutPathOverlap)
	require.Contains(t, log.String(), checkoutID)
	require.Contains(t, log.String(), destination)
}

func TestCheckoutBindingAdmissionPersistsFolderReuseAcrossRestart(t *testing.T) {
	t.Parallel()
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(layout.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "photo.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	destination := t.TempDir()
	checkout, operation, _, err := catalog.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, destination, 0)
	require.NoError(t, err)
	require.NoError(t, catalog.FinishPhotoCheckout(t.Context(), checkout.ID, operation.ID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	_, err = catalog.RetirePhotoCheckout(t.Context(), checkout.ID)
	require.NoError(t, err)
	binding := config.StoreBindingConfig{Kind: "filesystem", Path: destination}
	admit := newCheckoutBindingAdmission(catalog, slog.New(slog.DiscardHandler))
	require.NoError(t, admit(t.Context(), binding))
	require.NoError(t, blob.EnsureFilesystemNamespace(destination))
	require.NoError(t, catalog.Close())
	catalog, err = store.Open(layout.DBPath())
	require.NoError(t, err)
	admit = newCheckoutBindingAdmission(catalog, slog.New(slog.DiscardHandler))
	require.NoError(t, admit(t.Context(), binding), "a new daemon must accept the reused store folder")
}

func TestCheckoutBindingAdmissionCacheAndSnapshotChanges(t *testing.T) {
	t.Parallel()
	catalog, checkoutID, operationID, _ := newCheckoutReservationFixture(t)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	walks := 0
	admission := &checkoutBindingAdmission{
		catalog: catalog, logger: slog.New(slog.DiscardHandler),
		check: func(ctx context.Context, checkouts []store.PhotoCheckout, path string) error {
			walks++
			return validateCheckoutPathSnapshot(ctx, checkouts, path)
		},
	}
	binding := config.StoreBindingConfig{Kind: "filesystem", Path: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, os.Mkdir(binding.Path, 0o700))
	require.NoError(t, admission.admit(t.Context(), binding))
	require.NoError(t, admission.admit(t.Context(), binding))
	require.Equal(t, 1, walks, "refresh must reuse the successful separation check")
	changed := binding
	changed.Priority++
	require.NoError(t, admission.admit(t.Context(), changed))
	require.Equal(t, 2, walks)
	require.NoError(t, os.Rename(binding.Path, binding.Path+"-old"))
	require.NoError(t, os.Mkdir(binding.Path, 0o700))
	require.NoError(t, admission.admit(t.Context(), binding))
	require.Equal(t, 3, walks, "a replacement root requires a new walk")
	require.NoError(t, catalog.FinishPhotoCheckout(t.Context(), checkoutID, operationID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	_, err := catalog.RetirePhotoCheckout(t.Context(), checkoutID)
	require.NoError(t, err)
	require.NoError(t, admission.admit(t.Context(), binding))
	require.Equal(t, 4, walks, "retirement invalidates the successful snapshot")
}

func TestCheckoutBindingAdmissionRechecksSnapshotChangedDuringWalk(t *testing.T) {
	t.Parallel()
	catalog, checkoutID, _, _ := newCheckoutReservationFixture(t)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	original, err := catalog.PhotoCheckout(t.Context(), checkoutID)
	require.NoError(t, err)
	entries, err := catalog.PhotoCheckoutEntries(t.Context(), original.ID)
	require.NoError(t, err)
	binding := config.StoreBindingConfig{Kind: "filesystem", Path: t.TempDir()}
	changed := false
	var log bytes.Buffer
	admission := &checkoutBindingAdmission{
		catalog: catalog, logger: slog.New(slog.NewTextHandler(&log, nil)),
		check: func(ctx context.Context, checkouts []store.PhotoCheckout, path string) error {
			err := validateCheckoutPathSnapshot(ctx, checkouts, path)
			if !changed && err == nil {
				changed = true
				_, _, _, createErr := catalog.CreatePhotoCheckout(ctx, store.PhotoCheckoutSelection{AssetIDs: []string{entries[0].AssetID}}, binding.Path, 0)
				require.NoError(t, createErr)
			}
			return err
		},
	}
	require.ErrorIs(t, admission.admit(t.Context(), binding), errCheckoutPathOverlap)
	require.ErrorIs(t, admission.admit(t.Context(), binding), errCheckoutPathOverlap)
	require.Equal(t, 1, strings.Count(log.String(), "storage binding is waiting"))
}

func TestCheckoutBindingAdmissionWalkDoesNotBlockOtherBindings(t *testing.T) {
	t.Parallel()
	catalog, _, _, _ := newCheckoutReservationFixture(t)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blocked := config.StoreBindingConfig{Kind: "filesystem", Path: t.TempDir()}
	other := config.StoreBindingConfig{Kind: "filesystem", Path: t.TempDir()}
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	admission := &checkoutBindingAdmission{
		catalog: catalog, logger: slog.New(slog.DiscardHandler),
		check: func(ctx context.Context, checkouts []store.PhotoCheckout, path string) error {
			if path == blocked.Path {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}
			return validateCheckoutPathSnapshot(ctx, checkouts, path)
		},
	}
	done := make(chan error, 1)
	go func() { done <- admission.admit(ctx, blocked) }()
	<-entered
	otherDone := make(chan error, 1)
	go func() { otherDone <- admission.admit(t.Context(), other) }()
	select {
	case err := <-otherDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		<-otherDone
		t.Fatal("one binding's walk blocked another binding")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestCheckoutValidationBacksOffAndLogsChangedErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var log bytes.Buffer
		attempts := 0
		var times []time.Time
		done := make(chan error, 1)
		go func() {
			done <- waitForCheckoutValidation(ctx, func(context.Context) error {
				attempts++
				times = append(times, time.Now())
				if attempts > 6 {
					return errors.New("folder changed")
				}
				return errors.New("folder holds copies")
			}, slog.New(slog.NewTextHandler(&log, nil)))
		}()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		require.Equal(t, 1, strings.Count(log.String(), "folder holds copies"))
		require.Equal(t, 1, strings.Count(log.String(), "folder changed"))
		require.Less(t, attempts, 25, "a permanent wait must back off")
		require.Greater(t, times[2].Sub(times[1]), times[1].Sub(times[0]))
		require.Equal(t, 30*time.Second, times[len(times)-1].Sub(times[len(times)-2]))
	})
}
