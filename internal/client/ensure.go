//go:build unix

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

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
			if !requireVersion {
				return true
			}
			// Version strings cannot distinguish incompatible dev builds. The
			// runtime protocol revision covers the HTTP/runtime-record contract,
			// while the key check also rejects pre-key daemons. Ensure replaces
			// any incompatible daemon before handing a client to a data command.
			return info.Version == version.Version &&
				rec.Metadata[metaProtocolVersion] == daemonProtocolVersion &&
				rec.Metadata[metaAPIKey] != ""
		},
	}
}

// Find reports the live, responding docbank daemon (any version): daemon
// discovery for status/stop. NEVER auto-starts.
func Find(ctx context.Context, root string) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error) {
	rec, info, ok, err := discover(ctx, root, false)
	if err != nil {
		return rec, info, ok, fmt.Errorf("discovering daemon: %w", err)
	}
	return rec, info, ok, nil
}

// newClientFor authenticates with the key the daemon itself published in
// its runtime record (configured or ephemeral) rather than re-reading
// config.toml: the record's key is the one the running daemon actually
// enforces, which matters when a background daemon was started under an
// older config than the one on disk now.
func newClientFor(rec kitdaemon.RuntimeRecord) *Client {
	return New("http://"+rec.Address, rec.Metadata[metaAPIKey])
}

// WithLaunchLock serializes daemon auto-start with update's stop/install/
// restart window. Acquisition waits until the caller's context expires because
// an update may legitimately hold the per-user lock beyond startup's readiness
// timeout. Callers should re-run discovery inside fn before spawning.
func WithLaunchLock(ctx context.Context, root string, fn func() error) error {
	for {
		launch, err := (home.Layout{Root: root}).TryLockLaunch()
		if err == nil {
			defer func() { _ = launch.Release() }()
			return fn()
		}
		if !errors.Is(err, home.ErrVaultLocked) {
			return fmt.Errorf("acquiring daemon launch lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquiring daemon launch lock: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Ensure returns a client for a version- and protocol-matched daemon,
// starting (and if needed, replacing an incompatible) one. CLI commands call
// this.
func Ensure(ctx context.Context) (*Client, error) {
	layout, err := home.Resolve()
	if err != nil {
		return nil, err
	}
	res, err := EnsureDaemon(ctx, layout.Root)
	if err != nil {
		return nil, err
	}
	return newClientFor(res.Record), nil
}

// EnsureDaemon converges the vault on exactly one version- and
// protocol-matched daemon: it returns the running daemon when it is compatible
// with this binary, and otherwise — under the launch lock — stops it and starts
// a fresh one. `daemon start`, `daemon restart`, and the data commands'
// auto-start all share this path, so there is a single replacement policy and
// no command ever leaves a stale daemon behind.
func EnsureDaemon(ctx context.Context, root string) (EnsureResult, error) {
	var res EnsureResult
	rec, _, ok, err := discover(ctx, root, true)
	if err != nil {
		return res, fmt.Errorf("discovering daemon: %w", err)
	}
	if ok {
		res.Record = rec
		return res, nil
	}

	// Serialize racing starters; re-check under the lock.
	err = WithLaunchLock(ctx, root, func() error {
		rec, _, ok, err = discover(ctx, root, true)
		if err != nil {
			return fmt.Errorf("discovering daemon: %w", err)
		}
		if ok {
			res.Record = rec
			return nil
		}

		// Any live incompatible daemon blocks the vault lock: replace it.
		old, _, found, findErr := Find(ctx, root)
		if findErr != nil {
			return findErr
		}
		if found {
			if err := stopRecord(ctx, old); err != nil {
				return fmt.Errorf("stopping incompatible daemon (pid %d, %s): %w",
					old.PID, old.Version, err)
			}
			res.Replaced = &old
		}

		res.Record, err = Start(ctx, root)
		res.Started = err == nil
		return err
	})
	return res, err
}

// Start spawns a detached daemon and waits for a compatible ping.
func Start(ctx context.Context, root string) (kitdaemon.RuntimeRecord, error) {
	return start(ctx, root, true)
}

// StartAnyVersion spawns a detached daemon and waits for any docbank daemon.
// update uses this after replacing the executable, because the old updater
// process cannot know the new binary's version string at compile time.
func StartAnyVersion(ctx context.Context, root string) (kitdaemon.RuntimeRecord, error) {
	return start(ctx, root, false)
}

func start(ctx context.Context, root string, requireVersion bool) (kitdaemon.RuntimeRecord, error) {
	exe, err := os.Executable()
	if err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("resolving executable for daemon spawn: %w", err)
	}
	logFile, logPath, err := (home.Layout{Root: root}).OpenLaunchOutput()
	if err != nil {
		return kitdaemon.RuntimeRecord{}, err
	}
	defer func() { _ = logFile.Close() }()
	defer func() { _ = os.Remove(logPath) }()
	// DOCBANK_HOME is forced to root so a caller-supplied root (update's
	// restart path, tests) can never spawn a daemon on a different vault
	// than the one being discovered.
	childPID := 0
	err = kitdaemon.StartDetached(ctx, kitdaemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"daemon", "run"},
		Env:        append(os.Environ(), EnvBackgroundDaemon+"=1", "DOCBANK_HOME="+root),
		Stdout:     logFile,
		Stderr:     logFile,
		AfterStart: func(cmd *exec.Cmd) { childPID = cmd.Process.Pid },
	})
	if err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("spawning daemon: %w", err)
	}

	deadline := time.Now().Add(ensureTimeout)
	opts := discoverOptions(requireVersion)
	for time.Now().Before(deadline) {
		rec, _, ok, err := discoverWithOptions(ctx, root, opts)
		if err == nil && ok {
			return rec, nil
		}
		if childPID > 0 && !kitdaemon.ProcessAlive(childPID) {
			return kitdaemon.RuntimeRecord{}, daemonStartFailure(
				logFile, "daemon exited before becoming ready")
		}
		select {
		case <-ctx.Done():
			return kitdaemon.RuntimeRecord{}, fmt.Errorf("waiting for daemon: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return kitdaemon.RuntimeRecord{}, daemonStartFailure(
		logFile, fmt.Sprintf("daemon did not become ready within %s", ensureTimeout))
}

func daemonStartFailure(output *os.File, summary string) error {
	_ = output.Sync()
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("%s (reading bootstrap output: %w)", summary, err)
	}
	data, err := io.ReadAll(io.LimitReader(output, 64<<10))
	if err != nil {
		return fmt.Errorf("%s (reading bootstrap output: %w)", summary, err)
	}
	if detail := strings.TrimSpace(string(data)); detail != "" {
		return fmt.Errorf("%s: %s", summary, detail)
	}
	return errors.New(summary)
}

func discover(
	ctx context.Context, root string, requireVersion bool,
) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error) {
	return discoverWithOptions(ctx, root, discoverOptions(requireVersion))
}

func discoverWithOptions(
	ctx context.Context, root string, opts kitdaemon.DiscoverOptions,
) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error) {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return kitdaemon.RuntimeRecord{}, kitdaemon.PingInfo{}, false, nil
	}
	if err != nil {
		return kitdaemon.RuntimeRecord{}, kitdaemon.PingInfo{}, false,
			fmt.Errorf("checking daemon runtime directory: %w", err)
	}
	if !info.IsDir() {
		return kitdaemon.RuntimeRecord{}, kitdaemon.PingInfo{}, false,
			fmt.Errorf("daemon runtime path %s is not a directory", root)
	}
	rec, ping, ok, err := kitdaemon.Discover(ctx, RuntimeStore(root), opts)
	if err != nil {
		return rec, ping, ok, fmt.Errorf("scanning daemon runtime records: %w", err)
	}
	return rec, ping, ok, nil
}

// Stop gracefully stops the discovered daemon: token endpoint first,
// SIGTERM only when create_time still matches the recorded PID. Returns
// false when no daemon was running.
func Stop(ctx context.Context, root string) (bool, error) {
	rec, _, ok, err := Find(ctx, root)
	if err != nil || !ok {
		return false, err
	}
	return true, stopRecord(ctx, rec)
}

func stopRecord(ctx context.Context, rec kitdaemon.RuntimeRecord) error {
	c := newClientFor(rec)
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
