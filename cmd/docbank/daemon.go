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
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonlife"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
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
	blobs, err := blob.New(store.NewPackCatalog(s), layout.BlobsDir())
	if err != nil {
		return err
	}
	defer func() { _ = blobs.Close() }()
	// Exclusive lock holder: any stale tmp file is provably abandoned.
	if err := blobs.CleanTmp(); err != nil {
		return err
	}
	jobSupervisor := jobs.New(sigCtx, logger)
	operationGate := api.NewOperationGate()

	listener, err := kitdaemon.Listen(ctx, kitdaemon.Endpoint{
		Network: kitdaemon.NetworkTCP,
		Address: net.JoinHostPort(cfg.Server.BindAddr, strconv.Itoa(cfg.Server.APIPort)),
	})
	if err != nil {
		return fmt.Errorf("binding API listener: %w", err)
	}
	addr := listener.Addr().String()

	shutdownToken, err := randomHex32()
	if err != nil {
		_ = listener.Close()
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
			_ = listener.Close()
			return fmt.Errorf("generating ephemeral API key: %w", err)
		}
	}
	cfg.Server.APIKey = apiKey

	rtStore := client.RuntimeStore(layout.Root)
	recPath, err = rtStore.Write(client.NewRecord(addr, apiKey, shutdownToken))
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("writing daemon runtime record: %w", err)
	}
	// Register the job wait after store/blob cleanup so their resources remain
	// open until runners return. The earlier runtime cleanup remains last.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), daemonlife.JobDrainTimeout)
		defer cancel()
		if err := jobSupervisor.Shutdown(shutdownCtx); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
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

	var stopOnce sync.Once
	stopCh := make(chan struct{})
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }

	tracker := api.NewActivityTracker()
	srv := api.NewServer(api.Deps{
		Store: s, Blobs: blobs, VaultRoot: layout.Root, Cfg: cfg, Logger: logger,
		StartedAt: time.Now(), ShutdownToken: shutdownToken, Shutdown: stop, Tracker: tracker,
		Jobs: jobSupervisor, Gate: operationGate,
	})
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	if background && cfg.Server.IdleTimeout.Std() > 0 && len(cfg.Watches) == 0 {
		if err := jobSupervisor.Start("daemon.idle-timeout", func(ctx context.Context) error {
			idleWatch(ctx, tracker, cfg.Server.IdleTimeout.Std(), logger, stop)
			return nil
		}); err != nil {
			return fmt.Errorf("starting idle-timeout job: %w", err)
		}
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(listener) }()
	logger.Info("docbank daemon listening", "addr", addr, "pid", os.Getpid(), "background", background)

	select {
	case err := <-errCh:
		return fmt.Errorf("daemon API server: %w", err)
	case <-sigCtx.Done():
	case <-stopCh:
	}
	logger.Info("docbank daemon shutting down")
	jobSupervisor.Stop()
	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), daemonlife.HTTPDrainTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		// A timed-out drain means handlers may still be running. Force-close
		// their connections before the deferred store close and lock release,
		// and report the shutdown as unclean rather than pretending success.
		_ = httpSrv.Close()
		return fmt.Errorf("draining daemon requests: %w", err)
	}
	return nil
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
