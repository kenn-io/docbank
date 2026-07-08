//go:build unix

package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/version"
)

const ensureTimeout = 30 * time.Second

func probeOptions() kitdaemon.ProbeOptions {
	return kitdaemon.ProbeOptions{ExpectedService: Service, Timeout: 2 * time.Second}
}

func discoverOptions(requireVersion bool) kitdaemon.DiscoverOptions {
	return kitdaemon.DiscoverOptions{
		Probe:           probeOptions(),
		RequirePIDAlive: true,
		Accept: func(rec kitdaemon.RuntimeRecord, info kitdaemon.PingInfo) bool {
			if !createTimeMatches(rec) {
				return false
			}
			return !requireVersion || info.Version == version.Version
		},
	}
}

// Find reports the live, responding docbank daemon (any version): daemon
// discovery for status/stop. NEVER auto-starts.
func Find(ctx context.Context, root string) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error) {
	rec, info, ok, err := kitdaemon.Discover(ctx, RuntimeStore(root), discoverOptions(false))
	if err != nil {
		return rec, info, ok, fmt.Errorf("discovering daemon: %w", err)
	}
	return rec, info, ok, nil
}

func newClientFor(rec kitdaemon.RuntimeRecord, cfg config.Config) *Client {
	return New("http://"+rec.Address, cfg.Server.APIKey)
}

// Ensure returns a client for a version-matched daemon, starting (and if
// needed, replacing a version-mismatched) one. CLI commands call this.
func Ensure(ctx context.Context) (*Client, error) {
	layout, err := home.Resolve()
	if err != nil {
		return nil, err
	}
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	cfg, err := config.Load(layout.Root)
	if err != nil {
		return nil, err
	}

	rec, _, ok, err := kitdaemon.Discover(ctx, RuntimeStore(layout.Root), discoverOptions(true))
	if err != nil {
		return nil, fmt.Errorf("discovering daemon: %w", err)
	}
	if ok {
		return newClientFor(rec, cfg), nil
	}

	// Serialize racing starters; re-check under the lock.
	launch := flock.New(filepath.Join(layout.Root, "launch.lock"))
	lockCtx, cancel := context.WithTimeout(ctx, ensureTimeout)
	defer cancel()
	if _, err := launch.TryLockContext(lockCtx, 100*time.Millisecond); err != nil {
		return nil, fmt.Errorf("acquiring daemon launch lock: %w", err)
	}
	defer func() { _ = launch.Unlock() }()

	rec, _, ok, err = kitdaemon.Discover(ctx, RuntimeStore(layout.Root), discoverOptions(true))
	if err != nil {
		return nil, fmt.Errorf("discovering daemon: %w", err)
	}
	if ok {
		return newClientFor(rec, cfg), nil
	}

	// A live daemon with the wrong version blocks the vault lock: replace it.
	if old, _, found, _ := Find(ctx, layout.Root); found {
		if err := stopRecord(ctx, old, cfg); err != nil {
			return nil, fmt.Errorf("stopping version-mismatched daemon (pid %d, %s): %w",
				old.PID, old.Version, err)
		}
	}

	rec, err = Start(ctx, layout.Root)
	if err != nil {
		return nil, err
	}
	return newClientFor(rec, cfg), nil
}

// Start spawns a detached daemon and waits for a compatible ping.
func Start(ctx context.Context, root string) (kitdaemon.RuntimeRecord, error) {
	exe, err := os.Executable()
	if err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("resolving executable for daemon spawn: %w", err)
	}
	logPath := filepath.Join(root, "logs", "serve.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()
	// DOCBANK_HOME is forced to root so a caller-supplied root (update's
	// restart path, tests) can never spawn a daemon on a different vault
	// than the one being discovered.
	err = kitdaemon.StartDetached(ctx, kitdaemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"serve"},
		Env:        append(os.Environ(), EnvBackgroundDaemon+"=1", "DOCBANK_HOME="+root),
		Stdout:     logFile,
		Stderr:     logFile,
	})
	if err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("spawning daemon: %w", err)
	}

	deadline := time.Now().Add(ensureTimeout)
	for time.Now().Before(deadline) {
		rec, _, ok, err := kitdaemon.Discover(ctx, RuntimeStore(root), discoverOptions(true))
		if err == nil && ok {
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return kitdaemon.RuntimeRecord{}, fmt.Errorf("waiting for daemon: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return kitdaemon.RuntimeRecord{}, fmt.Errorf(
		"daemon did not become ready within %s; check %s", ensureTimeout, logPath)
}

// Stop gracefully stops the discovered daemon: token endpoint first,
// SIGTERM only when create_time still matches the recorded PID. Returns
// false when no daemon was running.
func Stop(ctx context.Context, root string) (bool, error) {
	layout := home.Layout{Root: root}
	cfg, err := config.Load(layout.Root)
	if err != nil {
		return false, err
	}
	rec, _, ok, err := Find(ctx, root)
	if err != nil || !ok {
		return false, err
	}
	return true, stopRecord(ctx, rec, cfg)
}

func stopRecord(ctx context.Context, rec kitdaemon.RuntimeRecord, cfg config.Config) error {
	c := newClientFor(rec, cfg)
	if token := rec.Metadata[metaShutdownToken]; token != "" {
		if err := c.Shutdown(ctx, token); err == nil {
			if waitDead(ctx, rec, 10*time.Second) {
				return nil
			}
		}
	}
	// Signal fallback only when the PID is provably still our daemon.
	if !createTimeMatches(rec) {
		return errors.New("daemon PID no longer matches its recorded create time; not signaling")
	}
	if err := syscall.Kill(rec.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signaling daemon pid %d: %w", rec.PID, err)
	}
	if !waitDead(ctx, rec, 10*time.Second) {
		return fmt.Errorf("daemon pid %d did not exit after SIGTERM", rec.PID)
	}
	return nil
}

func waitDead(ctx context.Context, rec kitdaemon.RuntimeRecord, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !kitdaemon.ProcessAlive(rec.PID) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return !kitdaemon.ProcessAlive(rec.PID)
}
