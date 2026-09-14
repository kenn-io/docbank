package api

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/loadfile"
)

type packageRootKey struct {
	owner       string
	preflightID string
}

type packageRootEntry struct {
	resolver  *loadfile.Resolver
	sourceRef string
	expiresAt time.Time
}

// packageRootRegistry retains the approved directory handle behind a root
// preflight. The owner-bound handle is deliberately process-local: restarting
// the daemon invalidates it and requires the caller to preflight again.
type packageRootRegistry struct {
	mu      sync.Mutex
	entries map[packageRootKey]packageRootEntry
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	closed  bool
}

func newPackageRootRegistry() *packageRootRegistry {
	registry := &packageRootRegistry{
		entries: make(map[packageRootKey]packageRootEntry),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go registry.sweep()
	return registry
}

func (r *packageRootRegistry) register(owner, preflightID, sourceRef, expiresAt string, resolver *loadfile.Resolver) error {
	if owner == "" || preflightID == "" || sourceRef == "" || resolver == nil {
		return errors.New("invalid approved package root")
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return fmt.Errorf("parse approved package root expiry: %w", err)
	}
	key := packageRootKey{owner: owner, preflightID: preflightID}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("approved package root registry is closed")
	}
	previous := r.entries[key]
	r.entries[key] = packageRootEntry{resolver: resolver, sourceRef: sourceRef, expiresAt: expires}
	r.mu.Unlock()
	if previous.resolver != nil && previous.resolver != resolver {
		_ = previous.resolver.Close()
	}
	return nil
}

func (r *packageRootRegistry) resolver(owner, preflightID, sourceRef string, now time.Time) (*loadfile.Resolver, bool) {
	key := packageRootKey{owner: owner, preflightID: preflightID}
	r.mu.Lock()
	entry, ok := r.entries[key]
	if ok && entry.sourceRef != sourceRef {
		r.mu.Unlock()
		return nil, false
	}
	if ok && !entry.expiresAt.After(now) {
		delete(r.entries, key)
		ok = false
	}
	r.mu.Unlock()
	if !ok {
		if entry.resolver != nil {
			_ = entry.resolver.Close()
		}
		return nil, false
	}
	return entry.resolver, true
}

func (r *packageRootRegistry) sweep() {
	ticker := time.NewTicker(time.Minute)
	defer func() {
		ticker.Stop()
		close(r.done)
	}()
	for {
		select {
		case now := <-ticker.C:
			r.expire(now)
		case <-r.stop:
			return
		}
	}
}

func (r *packageRootRegistry) expire(now time.Time) {
	var expired []*loadfile.Resolver
	r.mu.Lock()
	for key, entry := range r.entries {
		if !entry.expiresAt.After(now) {
			delete(r.entries, key)
			expired = append(expired, entry.resolver)
		}
	}
	r.mu.Unlock()
	for _, resolver := range expired {
		_ = resolver.Close()
	}
}

func (r *packageRootRegistry) closeAll() error {
	r.once.Do(func() { close(r.stop) })
	<-r.done
	r.mu.Lock()
	r.closed = true
	entries := r.entries
	r.entries = make(map[packageRootKey]packageRootEntry)
	r.mu.Unlock()
	var err error
	for _, entry := range entries {
		err = errors.Join(err, entry.resolver.Close())
	}
	return err
}
