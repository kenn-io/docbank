package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CompatibleFunc returns true when a discovered daemon can serve this client.
type CompatibleFunc func(RuntimeRecord, PingInfo) bool

// FindFunc returns a live compatible daemon using caller-defined discovery.
// It must honor ctx so Manager.Ensure can enforce its timeout. Callbacks that
// use bearer credentials must prove the runtime endpoint with Proof.Probe
// before sending them; ordinary authenticated discovery can use Discover.Proof.
type FindFunc func(context.Context) (RuntimeRecord, PingInfo, bool, error)

// StartFunc starts a daemon in the background.
type StartFunc func(context.Context) error

// Manager coordinates discovery and optional auto-start. When FindFunc is nil,
// Find uses Store, Discover, and Compatible. Store is still required by Ensure
// because it identifies the start lock, even when FindFunc supplies discovery.
type Manager struct {
	Store      RuntimeStore
	Discover   DiscoverOptions
	Compatible CompatibleFunc
	FindFunc   FindFunc
	Start      StartFunc
}

// Find returns a live compatible daemon, when one is already running. When
// FindFunc is set, Find delegates discovery to it.
func (m Manager) Find(ctx context.Context) (RuntimeRecord, PingInfo, bool, error) {
	if m.FindFunc != nil {
		return m.FindFunc(ctx)
	}
	opts := m.Discover
	accept := opts.Accept
	opts.Accept = func(rec RuntimeRecord, info PingInfo) bool {
		if accept != nil && !accept(rec, info) {
			return false
		}
		return m.Compatible == nil || m.Compatible(rec, info)
	}
	return Discover(ctx, m.Store, opts)
}

// Ensure returns a live compatible daemon, starting one when necessary.
func (m Manager) Ensure(ctx context.Context, timeout time.Duration) (RuntimeRecord, PingInfo, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	if rec, info, ok, err := m.Find(ctx); err != nil || ok {
		return rec, info, err
	}
	if m.Start == nil {
		return RuntimeRecord{}, PingInfo{}, errors.New("daemon not running")
	}
	unlock, err := m.lockStart(ctx)
	if err != nil {
		return RuntimeRecord{}, PingInfo{}, err
	}
	defer unlock()

	if rec, info, ok, err := m.Find(ctx); err != nil || ok {
		return rec, info, err
	}
	if err := m.Start(ctx); err != nil {
		return RuntimeRecord{}, PingInfo{}, err
	}
	var lastErr error
	for time.Now().Before(deadline) {
		rec, info, ok, err := m.Find(ctx)
		if err != nil {
			lastErr = err
		} else if ok {
			return rec, info, nil
		}
		select {
		case <-ctx.Done():
			return RuntimeRecord{}, PingInfo{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return RuntimeRecord{}, PingInfo{}, fmt.Errorf("daemon failed to start within %s: %w", timeout, lastErr)
	}
	return RuntimeRecord{}, PingInfo{}, fmt.Errorf("daemon failed to start within %s", timeout)
}

func (m Manager) lockStart(ctx context.Context) (func(), error) {
	return m.Store.AcquireStartLock(ctx)
}
