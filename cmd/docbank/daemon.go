package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	kitdaemon "go.kenn.io/kit/daemon"
	kitlogging "go.kenn.io/kit/logging"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonlife"
	"go.kenn.io/docbank/internal/extract"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/jobs"
	internalmaintenance "go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	docweb "go.kenn.io/docbank/internal/web"
)

var daemonRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the daemon in the foreground",
	Long: "Run the daemon in the foreground. Usually invoked by `docbank daemon start`\n" +
		"in the background; useful directly for debugging.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		err := runServe(cmd.Context())
		if err != nil && os.Getenv(client.EnvBackgroundDaemon) == "1" {
			_ = client.WriteDaemonStartProblem(cmd.ErrOrStderr(), err)
		}
		return err
	},
}

type embeddingRuntimeReadiness interface{ Ready() bool }

type embeddingJobRunner interface {
	Run(ctx context.Context) error
}

type embeddingJobStarter interface {
	Start(name string, run func(context.Context) error) error
}

// startEmbeddingWorkerIfReady preserves the daemon's normal supervisor-owned
// cancellation and drain lifecycle. An empty runtime registry leaves durable
// work untouched. A ready runtime must provide a real worker; configuration
// failures are not converted into a no-op polling loop.
func startEmbeddingWorkerIfReady(starter embeddingJobStarter, readiness embeddingRuntimeReadiness,
	build func() (embeddingJobRunner, error),
) error {
	if readiness == nil || !readiness.Ready() {
		return nil
	}
	worker, err := build()
	if err != nil {
		return err
	}
	if worker == nil {
		return errors.New("embedding worker builder returned nil")
	}
	return starter.Start("process:embeddings", worker.Run)
}

func startVectorIndexWorker(starter embeddingJobStarter,
	build func() (embeddingJobRunner, error),
) error {
	worker, err := build()
	if err != nil {
		return err
	}
	if worker == nil {
		return errors.New("vector index worker builder returned nil")
	}
	return starter.Start("process:vector-indexes", worker.Run)
}

func runServe(ctx context.Context) (retErr error) {
	layout, err := home.Resolve()
	if err != nil {
		return err
	}
	root, lock, err := layout.OpenAndLockExclusive()
	if err != nil {
		return err
	}
	// This is deliberately the first cleanup registered after acquiring the
	// vault. LIFO execution removes the runtime record only after supervised
	// jobs, storage, the lock, and the held root have all finished cleanup.
	var recPath string
	defer func() {
		if recPath != "" {
			_ = os.Remove(recPath)
		}
	}()
	defer func() { _ = root.Close() }()
	defer func() { _ = lock.Release() }()
	if err := docweb.RemoveBootstrap(layout.Root); err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, docweb.RemoveBootstrap(layout.Root)) }()

	if err := backupapp.RecoverInterruptedPrimaryHandoff(
		ctx, layout.Root, store.DefaultSQLiteDriver(),
	); err != nil {
		return err
	}
	if err := layout.Ensure(); err != nil {
		return err
	}
	cfg, err := config.Load(layout.Root)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	background := os.Getenv(client.EnvBackgroundDaemon) == "1"
	logger, loggingResult, err := buildServeLogger(layout, background)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, loggingResult.Close()) }()
	sigCtx, sigCancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer sigCancel()

	s, err := store.Open(layout.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	catalogStores, err := s.BlobStores(sigCtx)
	if err != nil {
		return err
	}
	if err := validateConfiguredWatchStores(cfg, catalogStores); err != nil {
		return err
	}
	storeSpecs := make([]blob.StoreSpec, 0, len(catalogStores)-1)
	for _, catalogStore := range catalogStores {
		if catalogStore.Role == "primary" {
			continue
		}
		storeSpecs = append(storeSpecs, blob.StoreSpec{
			ID: catalogStore.ID, Kind: catalogStore.Kind, Role: catalogStore.Role,
			Lifecycle: catalogStore.Lifecycle, Binding: catalogStore.Binding,
			OwnershipEpoch: catalogStore.OwnershipEpoch,
		})
	}
	blobRegistry := blob.NewRegistry(sigCtx, s.VaultID(), cfg.StoreBindings, storeSpecs)
	blobOptions := blob.ManagedOptions()
	blobOptions.Registry = blobRegistry
	blobs, err := blob.NewWithOptions(
		store.NewPackCatalog(s), layout.BlobsDir(), blobOptions,
	)
	if err != nil {
		_ = blobRegistry.Close()
		return err
	}
	defer func() { _ = blobs.Close() }()
	// Exclusive lock holder: any stale tmp file is provably abandoned.
	if err := recoverEmbeddingRuntimeSpool(sigCtx, layout.BlobTmpDir()); err != nil {
		return err
	}
	if err := blobs.CleanTmp(); err != nil {
		return err
	}
	jobSupervisor := jobs.New(sigCtx, logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), daemonlife.JobDrainTimeout)
		defer cancel()
		if err := jobSupervisor.Shutdown(shutdownCtx); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	operationGate := api.NewOperationGate()
	runtimeRegistry := processing.NewRenditionRuntimeRegistry()
	embeddingRuntimes, err := configureEmbeddingRuntimeBundle(cfg, blobs, layout.BlobTmpDir())
	if err != nil {
		return fmt.Errorf("configuring embedding runtimes: %w", err)
	}
	embeddingRuntimeRegistry := embeddingRuntimes.registry
	processingProfiles, err := executableProcessingProfiles(cfg, embeddingRuntimes)
	if err != nil {
		return fmt.Errorf("configuring executable processing profiles: %w", err)
	}
	processingService, err := processing.NewService(processing.ServiceConfig{
		Catalog: s, Blobs: blobs, Gate: operationGate, Profiles: processingProfiles,
		Principal: "daemon:operator", Scope: "document-processing", SpoolDirectory: layout.BlobTmpDir(),
	})
	if err != nil {
		return fmt.Errorf("configuring processing service: %w", err)
	}
	// Provider adapters register before this admission point. Until the daemon
	// has one, leave restored jobs untouched instead of repeatedly claiming and
	// delaying work that this process cannot execute.
	if runtimeRegistry.Ready() {
		renditionWorker, workerErr := processing.NewRenditionWorker(processing.RenditionWorkerConfig{
			Catalog: s, Blobs: blobs, Runtime: runtimeRegistry, Gate: operationGate,
			Owner: "daemon-rendition-worker", LeaseDuration: 5 * time.Minute,
			IdleDelay: time.Second,
		})
		if workerErr != nil {
			return fmt.Errorf("configuring rendition worker: %w", workerErr)
		}
		if err := jobSupervisor.Start("process:renditions", renditionWorker.Run); err != nil {
			return fmt.Errorf("starting rendition worker: %w", err)
		}
	}
	if err := startEmbeddingWorkerIfReady(jobSupervisor, embeddingRuntimeRegistry,
		func() (embeddingJobRunner, error) {
			worker, workerErr := processing.NewEmbeddingWorker(processing.EmbeddingWorkerConfig{
				Catalog: s, Authority: s, Blobs: blobs, GenerationBlobs: blobs, Runtime: embeddingRuntimeRegistry,
				Gate: operationGate, Owner: "daemon-embedding-worker",
				LeaseDuration: 5 * time.Minute, IdleDelay: time.Second,
				RetryLimit: 3, RetryBaseDelay: time.Second, MaxRetryDelay: 30 * time.Second,
				AttemptLifetime: 30 * time.Minute, MaxRows: 100_000,
				MaxDimensions: 1_048_576, MaxVectorBlobBytes: 64 << 20,
				DescriptorFingerprints: embeddingRuntimeRegistry.Fingerprints(),
			})
			if workerErr != nil {
				return nil, fmt.Errorf("configuring embedding worker: %w", workerErr)
			}
			return worker, nil
		}); err != nil {
		return err
	}
	if err := startVectorIndexWorker(jobSupervisor, func() (embeddingJobRunner, error) {
		worker, workerErr := processing.NewIndexWorker(processing.IndexWorkerConfig{
			Catalog: s, Blobs: blobs, Gate: operationGate,
			Owner: "daemon-vector-index-worker", BuildLease: 30 * time.Minute,
			ReaderLease: 5 * time.Minute, IdleDelay: time.Second,
		})
		if workerErr != nil {
			return nil, fmt.Errorf("configuring vector index worker: %w", workerErr)
		}
		return worker, nil
	}); err != nil {
		return err
	}
	if err := jobSupervisor.Start("maintenance:auxiliary-checksums", func(ctx context.Context) error {
		cursor := ""
		retries := newBackfillRetrySet()
		for {
			targets, listErr := retryDaemonList(ctx, 5*time.Second, func() (
				[]store.BlobChecksumTarget, error,
			) {
				return s.MissingBlobChecksumTargetsAfter(ctx, cursor, 100)
			}, func(err error) {
				logger.Warn("listing auxiliary checksum targets will retry", "error", err)
			})
			if listErr != nil {
				return listErr
			}
			if len(targets) == 0 {
				cursor = ""
				if len(retries) == 0 {
					return nil
				}
				if err := waitDaemonJob(ctx, retries.waitDelay(time.Now().UTC(), 10*time.Second)); err != nil {
					return err
				}
				continue
			}
			cursor = targets[len(targets)-1].BlobSHA256
			completed := 0
			attempted := 0
			err := operationGate.MutateContext(ctx, func() error {
				var batchErr error
				for _, target := range targets {
					now := time.Now().UTC()
					if !retries.ready(target.BlobSHA256, now) {
						continue
					}
					attempted++
					done, targetErr := processing.BackfillAuxiliaryChecksumTargets(
						ctx, s, blobs, []store.BlobChecksumTarget{target})
					completed += done
					if targetErr != nil {
						retries.failed(target.BlobSHA256, now)
						batchErr = errors.Join(batchErr, targetErr)
					} else {
						retries.succeeded(target.BlobSHA256)
					}
				}
				return batchErr
			})
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				logger.Warn("auxiliary checksum backfill will retry", "completed", completed, "error", err)
			}
			delay := backfillBatchWaitDelay(attempted, retries, time.Now().UTC())
			if err := waitDaemonJob(ctx, delay); err != nil {
				return err
			}
		}
	}); err != nil {
		return fmt.Errorf("starting auxiliary checksum backfill: %w", err)
	}
	if err := jobSupervisor.Start("extract:source-metadata", func(ctx context.Context) error {
		idleDelay := time.Second
		cursor := ""
		retries := newBackfillRetrySet()
		for {
			targets, listErr := retryDaemonList(ctx, 5*time.Second, func() (
				[]store.SourceMetadataTarget, error,
			) {
				return s.MissingSourceMetadataTargetsAfter(
					ctx, processing.SourceMetadataExtractorFingerprint, cursor, 10)
			}, func(err error) {
				logger.Warn("listing source metadata targets will retry", "error", err)
			})
			if listErr != nil {
				return listErr
			}
			if len(targets) == 0 {
				cursor = ""
				delay := retries.waitDelay(time.Now().UTC(), idleDelay)
				if err := waitDaemonJob(ctx, delay); err != nil {
					return err
				}
				idleDelay = min(idleDelay*2, 10*time.Second)
				continue
			}
			cursor = targets[len(targets)-1].SourceSHA256
			idleDelay = time.Second
			completed := 0
			attempted := 0
			err := operationGate.MutateContext(ctx, func() error {
				var batchErr error
				for _, target := range targets {
					now := time.Now().UTC()
					if !retries.ready(target.SourceSHA256, now) {
						continue
					}
					attempted++
					done, targetErr := processing.BackfillSourceMetadataTargets(
						ctx, s, blobs, []store.SourceMetadataTarget{target})
					completed += done
					if targetErr != nil {
						retries.failed(target.SourceSHA256, now)
						batchErr = errors.Join(batchErr, targetErr)
					} else {
						retries.succeeded(target.SourceSHA256)
					}
				}
				return batchErr
			})
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				logger.Warn("source metadata backfill will retry", "completed", completed, "error", err)
			}
			delay := backfillBatchWaitDelay(attempted, retries, time.Now().UTC())
			if err := waitDaemonJob(ctx, delay); err != nil {
				return err
			}
		}
	}); err != nil {
		return fmt.Errorf("starting source metadata backfill: %w", err)
	}
	placementRunner := blob.PlacementRunner{
		Metadata: s, Blobs: blobs, Commit: operationGate.PhysicalMutate,
	}
	if err := placementRunner.Resume(sigCtx, jobSupervisor); err != nil {
		return fmt.Errorf("resuming durable storage operations: %w", err)
	}

	listener, err := kitdaemon.Listen(ctx, kitdaemon.Endpoint{
		Network: kitdaemon.NetworkTCP,
		Address: net.JoinHostPort(cfg.Server.BindAddr, strconv.Itoa(cfg.Server.APIPort)),
	})
	if err != nil {
		return fmt.Errorf("binding API listener: %w", err)
	}
	defer func() { _ = listener.Close() }()
	addr := listener.Addr().String()

	webListener, webURL, err := listenWebOrigin(
		ctx, cfg.Web.Enabled && docweb.Available(),
	)
	if err != nil {
		return err
	}
	if webListener != nil {
		defer func() { _ = webListener.Close() }()
	}

	shutdownToken, err := randomHex32()
	if err != nil {
		return fmt.Errorf("generating shutdown token: %w", err)
	}

	// The daemon always requires an API key. A configured key is used as-is;
	// otherwise a fresh per-run key is generated and published only to
	// same-user clients via the runtime record inside owner-private DOCBANK_HOME
	// — never over the network, never logged.
	apiKey := cfg.Server.APIKey
	if apiKey == "" {
		apiKey, err = randomHex32()
		if err != nil {
			return fmt.Errorf("generating ephemeral API key: %w", err)
		}
	}
	cfg.Server.APIKey = apiKey

	rtStore := client.RuntimeStore(layout.Root)
	webAddress := ""
	if webListener != nil {
		webAddress = webListener.Addr().String()
	}
	recPath, err = rtStore.Write(client.NewRecord(addr, apiKey, shutdownToken, webAddress))
	if err != nil {
		return fmt.Errorf("writing daemon runtime record: %w", err)
	}
	for _, watchConfig := range cfg.Watches {
		watcher, err := ingest.NewWatcher(
			&ingest.Ingester{Store: s, Blobs: blobs}, layout.Root, watchConfig,
			operationGate.Mutate, logger,
		)
		if err != nil {
			return fmt.Errorf("configuring watch %q: %w", watchConfig.Name, err)
		}
		if err := jobSupervisor.Start("watch:"+watchConfig.Name, watcher.Run); err != nil {
			return fmt.Errorf("starting watch %q: %w", watchConfig.Name, err)
		}
	}
	textWorker, err := extract.New(s, blobs, operationGate.Mutate)
	if err != nil {
		return fmt.Errorf("configuring text extraction: %w", err)
	}
	if err := jobSupervisor.Start("extract:plain-text", textWorker.Run); err != nil {
		return fmt.Errorf("starting text extraction: %w", err)
	}
	if cfg.Storage.PackInterval.Std() > 0 {
		packRun := func(ctx context.Context) (internalmaintenance.PackReport, error) {
			var report internalmaintenance.PackReport
			err := operationGate.Maintain(func() error {
				var err error
				report, err = internalmaintenance.Pack(
					ctx, s, blobs, cfg.Storage.PackMaxBytes)
				return err
			})
			return report, err
		}
		if err := jobSupervisor.Start("storage:pack", func(ctx context.Context) error {
			return internalmaintenance.RunPackSchedule(
				ctx, cfg.Storage.PackInterval.Std(), packRun, logger)
		}); err != nil {
			return fmt.Errorf("starting automatic packing: %w", err)
		}
	}

	var stopOnce sync.Once
	stopCh := make(chan struct{})
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }

	tracker := api.NewActivityTracker()
	srv := api.NewServer(api.Deps{
		Store: s, Blobs: blobs, VaultRoot: layout.Root, Cfg: cfg, Logger: logger,
		StartedAt: time.Now(), ShutdownToken: shutdownToken, Shutdown: stop, Tracker: tracker,
		Jobs: jobSupervisor, Gate: operationGate, WebURL: webURL, BlobRegistry: blobRegistry,
		Processing: processingService,
	})
	defer srv.Close()
	newHTTPServer := func() *http.Server {
		return &http.Server{
			Handler:           srv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			BaseContext:       func(net.Listener) context.Context { return ctx },
		}
	}
	type servingHTTP struct {
		name     string
		server   *http.Server
		listener net.Listener
	}
	servers := []servingHTTP{{name: "API", server: newHTTPServer(), listener: listener}}
	if webListener != nil {
		servers = append(servers,
			servingHTTP{name: "web", server: newHTTPServer(), listener: webListener})
	}

	if background && cfg.Server.IdleTimeout.Std() > 0 && len(cfg.Watches) == 0 &&
		cfg.Storage.PackInterval.Std() == 0 {
		if err := jobSupervisor.Start("daemon.idle-timeout", func(ctx context.Context) error {
			idleWatch(ctx, tracker, cfg.Server.IdleTimeout.Std(), logger, stop)
			return nil
		}); err != nil {
			return fmt.Errorf("starting idle-timeout job: %w", err)
		}
	}

	type serveResult struct {
		name string
		err  error
	}
	errCh := make(chan serveResult, len(servers))
	for _, running := range servers {
		go func() {
			errCh <- serveResult{name: running.name, err: running.server.Serve(running.listener)}
		}()
	}
	logger.Info("docbank daemon listening", "addr", addr, "pid", os.Getpid(), "background", background)
	if webListener != nil {
		logger.Info("docbank web application listening", "addr", webListener.Addr().String())
	}

	var serveErr error
	select {
	case result := <-errCh:
		if !errors.Is(result.err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("daemon %s server: %w", result.name, result.err)
		}
	case <-sigCtx.Done():
	case <-stopCh:
	}
	logger.Info("docbank daemon shutting down")
	jobSupervisor.Stop()
	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), daemonlife.HTTPDrainTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	for _, running := range servers {
		if err := running.server.Shutdown(shutdownCtx); err != nil {
			_ = running.server.Close()
			shutdownErr = errors.Join(shutdownErr,
				fmt.Errorf("draining daemon %s requests: %w", running.name, err))
		}
	}
	if shutdownErr != nil {
		// A timed-out drain means handlers may still be running. Force-close
		// their connections before the deferred store close and lock release,
		// and report the shutdown as unclean rather than pretending success.
		return shutdownErr
	}
	return serveErr
}

func retryDaemonList[T any](
	ctx context.Context, retryDelay time.Duration, list func() ([]T, error), onRetry func(error),
) ([]T, error) {
	for {
		items, err := list()
		if err == nil {
			return items, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		onRetry(err)
		if err := waitDaemonJob(ctx, retryDelay); err != nil {
			return nil, err
		}
	}
}

func waitDaemonJob(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateConfiguredWatchStores(
	cfg config.Config, stores []store.BlobStore,
) error {
	for _, item := range stores {
		if item.Role == "primary" || item.Lifecycle == "detached" ||
			item.Kind != "filesystem" {
			continue
		}
		binding, ok := cfg.StoreBindings[item.Binding]
		if !ok {
			continue
		}
		watchName, overlap, err := ingest.WatchBindingOverlap(cfg.Watches, binding)
		if err != nil {
			return fmt.Errorf(
				"checking watch sources against filesystem store %q: %w", item.Name, err,
			)
		}
		if overlap {
			return fmt.Errorf(
				"watch %q source overlaps filesystem store %q binding %q",
				watchName, item.Name, item.Binding,
			)
		}
	}
	return nil
}

func listenWebOrigin(ctx context.Context, enabled bool) (net.Listener, string, error) {
	if !enabled {
		return nil, "", nil
	}
	identity, err := randomHex32()
	if err != nil {
		return nil, "", fmt.Errorf("generating web origin identity: %w", err)
	}
	return listenWebOriginWithIdentity(ctx, identity[:32])
}

func listenWebOriginWithIdentity(
	ctx context.Context, identity string,
) (net.Listener, string, error) {
	listener, err := kitdaemon.Listen(ctx, kitdaemon.Endpoint{
		Network: kitdaemon.NetworkTCP,
		Address: net.JoinHostPort("127.0.0.1", "0"),
	})
	if err != nil {
		return nil, "", fmt.Errorf("binding web listener: %w", err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, "", fmt.Errorf("reading web listener address: %w", err)
	}
	origin := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("docbank-"+identity+".localhost", port),
		Path:   "/",
	}
	return listener, origin.String(), nil
}

// idleWatch exits an auto-started daemon after a fully quiet window so
// spawned daemons don't accumulate. Foreground `daemon run` never idles out.
func idleWatch(ctx context.Context, t *api.ActivityTracker, timeout time.Duration,
	logger *slog.Logger, stop func()) {
	// Clamp the poll interval: NewTicker panics on a non-positive duration,
	// and a pathologically small configured timeout must not spin.
	interval := max(timeout/10, 50*time.Millisecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if t.IdleFor() >= timeout {
				logger.Info("idle timeout reached, shutting down", "idle", timeout.String())
				stop()
				return
			}
		}
	}
}

func buildServeLogger(
	layout home.Layout, background bool,
) (*slog.Logger, *kitlogging.Result, error) {
	logger, result, err := kitlogging.NewLogger(kitlogging.Options{
		Stderr:      os.Stderr,
		EnvLevelVar: "DOCBANK_LOG_LEVEL",
		File: kitlogging.FileOptions{
			Enabled:         background,
			Dir:             layout.LogsDir(),
			DailyFilePrefix: "docbank",
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("building daemon logger: %w", err)
	}
	return logger, result, nil
}

// randomHex32 returns a fresh 32-byte value hex-encoded, used for both the
// shutdown token and an ephemeral API key.
func randomHex32() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
