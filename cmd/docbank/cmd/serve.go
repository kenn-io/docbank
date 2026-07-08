package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the docbank daemon in the foreground",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return runServe(cmd.Context()) },
}

func runServe(ctx context.Context) error {
	layout, err := home.Resolve()
	if err != nil {
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
	logger, err := buildServeLogger(layout, background)
	if err != nil {
		return err
	}

	lock, err := layout.TryLockExclusive()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	s, err := store.Open(layout.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	blobs := blob.New(layout.BlobsDir())
	// Exclusive lock holder: any stale tmp file is provably abandoned.
	if err := blobs.CleanTmp(); err != nil {
		return err
	}

	listener, err := kitdaemon.Listen(ctx, kitdaemon.Endpoint{
		Network: kitdaemon.NetworkTCP,
		Address: net.JoinHostPort(cfg.Server.BindAddr, strconv.Itoa(cfg.Server.APIPort)),
	})
	if err != nil {
		return fmt.Errorf("binding API listener: %w", err)
	}
	addr := listener.Addr().String()

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		_ = listener.Close()
		return fmt.Errorf("generating shutdown token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	rtStore := client.RuntimeStore(layout.Root)
	recPath, err := rtStore.Write(client.NewRecord(addr, token))
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("writing daemon runtime record: %w", err)
	}
	defer func() { _ = os.Remove(recPath) }()

	var stopOnce sync.Once
	stopCh := make(chan struct{})
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }

	tracker := api.NewActivityTracker()
	srv := api.NewServer(api.Deps{
		Store: s, Blobs: blobs, Cfg: cfg, Logger: logger,
		StartedAt: time.Now(), ShutdownToken: token, Shutdown: stop, Tracker: tracker,
	})
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	sigCtx, sigCancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer sigCancel()

	if background && cfg.Server.IdleTimeout.Std() > 0 {
		go idleWatch(sigCtx, tracker, cfg.Server.IdleTimeout.Std(), logger, stop)
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
// spawned daemons don't accumulate. Foreground serves never idle out.
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

func buildServeLogger(layout home.Layout, background bool) (*slog.Logger, error) {
	logger, _, err := kitlogging.NewLogger(kitlogging.Options{
		Stderr:      os.Stderr,
		EnvLevelVar: "DOCBANK_LOG_LEVEL",
		File: kitlogging.FileOptions{
			Enabled:         background,
			Dir:             layout.LogsDir(),
			DailyFilePrefix: "docbank",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("building daemon logger: %w", err)
	}
	return logger, nil
}

func init() { rootCmd.AddCommand(serveCmd) }
