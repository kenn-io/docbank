package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonlife"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/version"
)

const ensureTimeout = 30 * time.Second

const (
	daemonStartProblemPrefix      = "DOCBANK_DAEMON_START_PROBLEM="
	daemonStartProblemVaultLocked = "vault_locked"
)

// ErrTransientDaemonAcquisition marks a daemon that disappeared after
// discovery but before its ownership-proven client was ready. A caller may
// safely repeat the complete discovery/start/proof sequence.
var ErrTransientDaemonAcquisition = errors.New("daemon acquisition was interrupted")

type daemonStartError struct {
	message string
	cause   error
}

func (e *daemonStartError) Error() string { return e.message }
func (e *daemonStartError) Unwrap() error { return e.cause }

// WriteDaemonStartProblem records a machine-readable cause in the launcher's
// private bootstrap output. The detached child cannot return Go error identity
// across a process boundary, so the launcher restores the small stable subset
// needed by callers before it returns the human error.
func WriteDaemonStartProblem(w io.Writer, err error) error {
	if !errors.Is(err, home.ErrVaultLocked) {
		return nil
	}
	if _, err := fmt.Fprintln(w, daemonStartProblemPrefix+daemonStartProblemVaultLocked); err != nil {
		return fmt.Errorf("writing daemon startup problem: %w", err)
	}
	return nil
}

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

// newProvenClientFor authenticates with the key the daemon itself published in
// its runtime record (configured or ephemeral) rather than re-reading
// config.toml. Before the key can cross the socket, the endpoint must prove it
// owns that private record, and every later request stays on the proven socket.
func newProvenClientFor(ctx context.Context, rec kitdaemon.RuntimeRecord) (*Client, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeOptions().Timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, kitdaemon.NetworkTCP, rec.Address)
	if err != nil {
		return nil, fmt.Errorf("dialing daemon for ownership proof: %w", err)
	}
	dialer := &singleConnDialer{conn: conn}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         dialer.DialContext,
		MaxConnsPerHost:     1,
		MaxIdleConnsPerHost: 1,
	}
	hc := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("daemon requests must not redirect")
		},
	}
	owned, proofErr := proveOwnershipWithClient(ctx, rec, hc)
	if proofErr != nil || !owned {
		transport.CloseIdleConnections()
		_ = conn.Close()
		if proofErr != nil {
			return nil, proofErr
		}
		return nil, errors.New("daemon endpoint failed ownership proof")
	}
	c := New("http://"+rec.Address, rec.Metadata[metaAPIKey])
	c.hc = hc
	return c, nil
}

// singleConnDialer gives the HTTP transport exactly the socket that completed
// ownership proof. If that connection closes, requests fail rather than
// reconnecting to a process that captured the loopback port.
type singleConnDialer struct {
	mu   sync.Mutex
	conn net.Conn
}

func (d *singleConnDialer) DialContext(
	_ context.Context, _, _ string,
) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil, errors.New("proven daemon connection is closed; refusing to redial")
	}
	conn := d.conn
	d.conn = nil
	return conn, nil
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
	c, err := newProvenClientFor(ctx, res.Record)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTransientDaemonAcquisition, err)
	}
	return c, nil
}

// EnsureDaemon converges the vault on exactly one version- and
// protocol-matched daemon: it returns the running daemon when it is compatible
// with this binary, and otherwise — under the launch lock — stops it and starts
// a fresh one. `daemon start`, `daemon restart`, and the data commands'
// auto-start all share this path, so there is a single replacement policy and
// no command ever leaves a stale daemon behind.
func EnsureDaemon(ctx context.Context, root string) (EnsureResult, error) {
	return ensureDaemon(ctx, root, Start)
}

func ensureDaemon(
	ctx context.Context,
	root string,
	startFn func(context.Context, string) (kitdaemon.RuntimeRecord, error),
) (EnsureResult, error) {
	var res EnsureResult
	root, err := home.CanonicalRoot(root)
	if err != nil {
		return res, err
	}
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
		} else {
			// Shutdown closes the listener before background jobs finish. During
			// that window ping-based discovery returns no daemon even though the
			// verified runtime PID still owns the vault. Wait for that owner (and
			// only force it after the complete graceful budget) before spawning.
			stopping, live, liveErr := liveRuntimeRecord(root)
			if liveErr != nil {
				return liveErr
			}
			if live {
				// A pingless endpoint is not authenticated ownership evidence: once
				// the recorded listener closes, another local process can claim its
				// port and forge the public ping fields. Stop only the identity-checked
				// recorded PID, then replace it after exit; never send its API key to a
				// listener that appeared during this transition.
				if err := signalStopRecord(ctx, stopping); err != nil {
					return fmt.Errorf("stopping pingless daemon (pid %d): %w",
						stopping.PID, err)
				}
				res.Replaced = &stopping
			}
		}

		res.Record, err = startFn(ctx, root)
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
	root, err := home.CanonicalRoot(root)
	if err != nil {
		return kitdaemon.RuntimeRecord{}, err
	}
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
	detail, cause := parseDaemonStartOutput(string(data))
	message := summary
	if detail != "" {
		message += ": " + detail
	}
	if cause != nil {
		return &daemonStartError{message: message, cause: cause}
	}
	return errors.New(message)
}

func parseDaemonStartOutput(output string) (string, error) {
	lines := strings.Split(output, "\n")
	kept := lines[:0]
	var cause error
	for _, line := range lines {
		switch strings.TrimSpace(line) {
		case daemonStartProblemPrefix + daemonStartProblemVaultLocked:
			cause = home.ErrVaultLocked
		default:
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), cause
}

func discover(
	ctx context.Context, root string, requireVersion bool,
) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error) {
	return discoverWithOptions(ctx, root, discoverOptions(requireVersion))
}

func discoverWithOptions(
	ctx context.Context, root string, opts kitdaemon.DiscoverOptions,
) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error) {
	root, err := home.CanonicalRoot(root)
	if err != nil {
		return kitdaemon.RuntimeRecord{}, kitdaemon.PingInfo{}, false, err
	}
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
	if ok {
		owned, proofErr := proveEndpointOwnership(ctx, rec)
		if proofErr != nil {
			return rec, ping, false, proofErr
		}
		if !owned {
			return rec, ping, false, nil
		}
	}
	return rec, ping, ok, nil
}

// proveEndpointOwnership binds public ping to the owner-private runtime record
// without transmitting its API key or shutdown token. A listener that captured
// the port during teardown can copy ping fields but cannot forge this response.
func proveEndpointOwnership(ctx context.Context, rec kitdaemon.RuntimeRecord) (bool, error) {
	challengeClient := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("daemon ownership challenge must not redirect")
		},
	}
	return proveOwnershipWithClient(ctx, rec, challengeClient)
}

func proveOwnershipWithClient(
	ctx context.Context, rec kitdaemon.RuntimeRecord, challengeClient *http.Client,
) (bool, error) {
	token := rec.Metadata[metaShutdownToken]
	if token == "" {
		return false, nil
	}
	nonce := make([]byte, daemonauth.NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return false, fmt.Errorf("generating daemon ownership challenge: %w", err)
	}
	u := url.URL{Scheme: "http", Host: rec.Address, Path: daemonauth.ChallengePath}
	query := u.Query()
	query.Set("nonce", hex.EncodeToString(nonce))
	u.RawQuery = query.Encode()
	probeCtx, cancel := context.WithTimeout(ctx, probeOptions().Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, fmt.Errorf("building daemon ownership challenge: %w", err)
	}
	resp, err := challengeClient.Do(req)
	if err != nil {
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return false, nil
	}
	var result struct {
		Proof string `json:"proof"`
	}
	limited := io.LimitReader(resp.Body, 4<<10)
	if err := json.NewDecoder(limited).Decode(&result); err != nil {
		return false, nil
	}
	_, _ = io.Copy(io.Discard, limited)
	return daemonauth.Verify(token, nonce, result.Proof), nil
}

// Stop gracefully stops the discovered daemon: token endpoint first,
// SIGTERM only when create_time still matches the recorded PID. Returns
// false when no daemon was running.
func Stop(ctx context.Context, root string) (bool, error) {
	root, err := home.CanonicalRoot(root)
	if err != nil {
		return false, err
	}
	rec, _, ok, err := Find(ctx, root)
	if err != nil {
		return false, err
	}
	if ok {
		return true, stopRecord(ctx, rec)
	}
	// A daemon that already closed its listener can still be draining jobs.
	// Recognize only a create-time-verified runtime PID without ping evidence.
	rec, ok, err = liveRuntimeRecord(root)
	if err != nil || !ok {
		return false, err
	}
	// Do not probe or send secrets to a listener that may have been rebound
	// after the recorded daemon stopped answering. Signal only the verified PID.
	return true, signalStopRecord(ctx, rec)
}

func stopRecord(ctx context.Context, rec kitdaemon.RuntimeRecord) error {
	c, proofErr := newProvenClientFor(ctx, rec)
	if proofErr != nil {
		// Endpoint ownership could not be proven without exposing credentials.
		// Signal only the create-time-verified process recorded in private state.
		return signalStopRecord(ctx, rec)
	}
	if token := rec.Metadata[metaShutdownToken]; token != "" {
		// A transport failure can mean another caller already closed the listener,
		// so preserve the drain budget. A completed HTTP rejection proves this
		// request did not initiate shutdown and uses the process-signal fallback.
		shutdownErr := c.Shutdown(ctx, token)
		if _, rejected := responseStatus(shutdownErr); rejected {
			return signalStopRecord(ctx, rec)
		}
		dead, err := waitDead(ctx, rec, daemonlife.GracefulExitTimeout)
		if err != nil {
			return fmt.Errorf("waiting for graceful daemon shutdown: %w", err)
		}
		if dead {
			return nil
		}
		return forceStopRecord(ctx, rec)
	}
	return signalStopRecord(ctx, rec)
}

func signalStopRecord(ctx context.Context, rec kitdaemon.RuntimeRecord) error {
	// Older records without a token, and definitive HTTP rejections, need the
	// platform's graceful signal. Windows has no process-scoped equivalent for a
	// detached daemon, so its request is a no-op and the complete grace window
	// elapses before force termination.
	if err := verifyRecordProcess(rec); err != nil {
		return err
	}
	if err := requestProcessStop(rec.PID); err != nil {
		return fmt.Errorf("requesting daemon pid %d stop: %w", rec.PID, err)
	}
	dead, err := waitDead(ctx, rec, daemonlife.GracefulExitTimeout)
	if err != nil {
		return fmt.Errorf("waiting for signaled daemon shutdown: %w", err)
	}
	if dead {
		return nil
	}
	return forceStopRecord(ctx, rec)
}

func forceStopRecord(ctx context.Context, rec kitdaemon.RuntimeRecord) error {
	if err := verifyRecordProcess(rec); err != nil {
		return err
	}
	if err := forceTerminateProcess(rec.PID); err != nil {
		return fmt.Errorf("forcibly terminating daemon pid %d: %w", rec.PID, err)
	}
	dead, err := waitDead(ctx, rec, daemonlife.ForcedExitTimeout)
	if err != nil {
		return fmt.Errorf("waiting for daemon after forced termination: %w", err)
	}
	if !dead {
		return fmt.Errorf("daemon pid %d did not exit after forced termination", rec.PID)
	}
	return nil
}

func verifyRecordProcess(rec kitdaemon.RuntimeRecord) error {
	if !createTimeMatches(rec) {
		return errors.New("daemon PID no longer matches its recorded create time; not signaling")
	}
	return nil
}

// liveRuntimeRecord finds a non-responsive daemon only when its owner-private
// record carries a create time that still matches a live process. Ping-less
// records without that proof are never trusted for waiting or signaling.
func liveRuntimeRecord(root string) (kitdaemon.RuntimeRecord, bool, error) {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return kitdaemon.RuntimeRecord{}, false, nil
	}
	if err != nil {
		return kitdaemon.RuntimeRecord{}, false,
			fmt.Errorf("checking daemon runtime directory: %w", err)
	}
	if !info.IsDir() {
		return kitdaemon.RuntimeRecord{}, false,
			fmt.Errorf("daemon runtime path %s is not a directory", root)
	}
	records, err := RuntimeStore(root).List()
	if err != nil {
		return kitdaemon.RuntimeRecord{}, false,
			fmt.Errorf("listing daemon runtime records: %w", err)
	}
	for _, rec := range records {
		if rec.Service != Service || rec.Metadata[metaCreateTime] == "" ||
			!kitdaemon.ProcessAlive(rec.PID) || !createTimeMatches(rec) {
			continue
		}
		return rec, true, nil
	}
	return kitdaemon.RuntimeRecord{}, false, nil
}

func waitDead(
	ctx context.Context, rec kitdaemon.RuntimeRecord, timeout time.Duration,
) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !kitdaemon.ProcessAlive(rec.PID) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return !kitdaemon.ProcessAlive(rec.PID), nil
}
