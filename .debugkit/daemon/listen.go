package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

const defaultStaleSocketProbeTimeout = 500 * time.Millisecond

type listenOptions struct {
	// Store provides the daemon runtime listen lock used to serialize Unix
	// socket stale cleanup and bind. Ignored when LockPath is set.
	store RuntimeStore

	// LockPath overrides the lock file used for Unix socket startup.
	lockPath string

	// StaleSocketProbeTimeout bounds the local dial used to prove that an
	// existing Unix socket path is stale before removing it.
	staleSocketProbeTimeout time.Duration
}

// ListenOption configures Listen.
type ListenOption func(*listenOptions)

// WithRuntimeStore uses store's daemon listen lock to serialize Unix socket
// stale cleanup and bind. Ignored when WithListenLockPath is also supplied.
func WithRuntimeStore(store RuntimeStore) ListenOption {
	return func(opts *listenOptions) {
		opts.store = store
	}
}

// WithListenLockPath overrides the lock file used for Unix socket startup.
func WithListenLockPath(lockPath string) ListenOption {
	return func(opts *listenOptions) {
		opts.lockPath = lockPath
	}
}

// WithStaleSocketProbeTimeout bounds the local dial used to prove that an
// existing Unix socket path is stale before removing it.
func WithStaleSocketProbeTimeout(timeout time.Duration) ListenOption {
	return func(opts *listenOptions) {
		opts.staleSocketProbeTimeout = timeout
	}
}

// Listen binds ep for daemon serving.
//
// For Unix sockets, Listen serializes stale socket probing/removal and the
// subsequent bind under an inter-process lock. Existing live sockets and
// non-socket paths are rejected. TCP endpoints retain Endpoint's normal Listen
// behavior.
func Listen(ctx context.Context, ep Endpoint, options ...ListenOption) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts := listenOptions{}
	for _, option := range options {
		option(&opts)
	}
	if !ep.IsUnix() {
		return ep.ListenContext(ctx)
	}
	if err := prepareUnixListenEndpoint(ep); err != nil {
		return nil, err
	}
	lockPath, err := opts.listenLockPath(ep)
	if err != nil {
		return nil, err
	}
	unlock, err := acquireDaemonLock(ctx, lockPath, "acquire daemon listen lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := removeStaleUnixSocket(ctx, ep, opts); err != nil {
		return nil, err
	}
	return ep.ListenContext(ctx)
}

func prepareUnixListenEndpoint(ep Endpoint) error {
	if ep.Address == "" {
		return errors.New("empty daemon endpoint address")
	}
	if !filepath.IsAbs(ep.Address) {
		return fmt.Errorf("unix socket path %q must be absolute", ep.Address)
	}
	if err := validatePrivateRuntimeDir(filepath.Dir(ep.Address)); err != nil {
		return fmt.Errorf("validate unix socket dir: %w", err)
	}
	return nil
}

func (opts listenOptions) listenLockPath(ep Endpoint) (string, error) {
	if opts.lockPath != "" {
		return opts.lockPath, nil
	}
	if opts.store.Dir != "" {
		return opts.store.ListenLockPath()
	}
	if ep.Address == "" {
		return "", errors.New("empty daemon endpoint address")
	}
	return ep.Address + ".lock", nil
}

func (opts listenOptions) staleProbeTimeout() time.Duration {
	if opts.staleSocketProbeTimeout > 0 {
		return opts.staleSocketProbeTimeout
	}
	return defaultStaleSocketProbeTimeout
}

func removeStaleUnixSocket(ctx context.Context, ep Endpoint, opts listenOptions) error {
	if ep.Address == "" {
		return errors.New("empty daemon endpoint address")
	}
	info, err := os.Lstat(ep.Address)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect unix socket %s: %w", ep.Address, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s", ep.Address)
	}
	stale, err := unixSocketStale(ctx, ep.Address, opts.staleProbeTimeout())
	if err != nil {
		return err
	}
	if !stale {
		return fmt.Errorf("daemon already listening on unix socket %s", ep.Address)
	}
	if err := os.Remove(ep.Address); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale unix socket %s: %w", ep.Address, err)
	}
	return nil
}

func unixSocketStale(ctx context.Context, path string, timeout time.Duration) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, NetworkUnix, path)
	if err == nil {
		_ = conn.Close()
		return false, nil
	}
	if ctxErr := probeCtx.Err(); ctxErr != nil {
		return false, fmt.Errorf("probe unix socket %s: %w", path, ctxErr)
	}
	if isStaleUnixSocketDialError(err) {
		return true, nil
	}
	return false, fmt.Errorf("probe unix socket %s: %w", path, err)
}
