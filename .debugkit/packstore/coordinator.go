package packstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrLeaseReleased reports a lease that is nil, invalid, or already released.
var ErrLeaseReleased = errors.New("packstore: lease already released")

// ErrWrongLeaseKind reports a live lease of a different kind than required.
var ErrWrongLeaseKind = errors.New("packstore: wrong lease kind")

// Coordinator serializes maintenance against application mutations while
// allowing independent mutations to proceed concurrently.
//
// Coordinator is process-local. Applications remain responsible for excluding
// other processes that could mutate the same catalog or blob directories.
// Maintenance waiters receive priority over mutations that arrive later.
// Callers must not upgrade or reenter a lease; acquire application-level gates
// before acquiring a Coordinator lease, and release leases promptly.
type Coordinator struct {
	mu                 sync.Mutex
	changed            chan struct{}
	activeMutations    int
	maintenanceActive  bool
	waitingMaintenance int
}

// NewCoordinator constructs a Coordinator.
func NewCoordinator() *Coordinator {
	return &Coordinator{changed: make(chan struct{})}
}

// Lease represents an acquired mutation or maintenance slot.
type Lease struct {
	state *leaseState
}

type leaseState struct {
	coordinator *Coordinator
	kind        leaseKind
	released    atomic.Bool
}

type leaseKind uint8

const (
	mutationLease leaseKind = iota
	maintenanceLease
)

// Validate reports whether the lease is live and may still protect work.
func (l *Lease) Validate() error {
	if l == nil || l.state == nil || l.state.coordinator == nil ||
		(l.state.kind != mutationLease && l.state.kind != maintenanceLease) ||
		l.state.released.Load() {
		return ErrLeaseReleased
	}
	return nil
}

// ValidateMutation reports whether the lease is a live mutation lease.
func (l *Lease) ValidateMutation() error {
	if err := l.Validate(); err != nil {
		return err
	}
	if l.state.kind != mutationLease {
		return ErrWrongLeaseKind
	}
	return nil
}

// AcquireMutation waits until no maintenance lease is active or queued.
func (c *Coordinator) AcquireMutation(ctx context.Context) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !c.maintenanceActive && c.waitingMaintenance == 0 {
			c.activeMutations++
			return &Lease{state: &leaseState{coordinator: c, kind: mutationLease}}, nil
		}

		changed := c.changedLocked()
		c.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		c.mu.Lock()
	}
}

// AcquireMaintenance waits for exclusive access against all Coordinator
// leases. Once queued, it prevents later mutation acquisitions from bypassing
// it.
func (c *Coordinator) AcquireMaintenance(ctx context.Context) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.waitingMaintenance++
	for {
		if err := ctx.Err(); err != nil {
			c.waitingMaintenance--
			c.notifyLocked()
			c.mu.Unlock()
			return nil, err
		}
		if !c.maintenanceActive && c.activeMutations == 0 {
			c.waitingMaintenance--
			c.maintenanceActive = true
			c.mu.Unlock()
			return &Lease{state: &leaseState{coordinator: c, kind: maintenanceLease}}, nil
		}

		changed := c.changedLocked()
		c.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		c.mu.Lock()
	}
}

// Release relinquishes the lease. A lease may be released exactly once.
func (l *Lease) Release() error {
	if l == nil || l.state == nil || !l.state.released.CompareAndSwap(false, true) {
		return ErrLeaseReleased
	}

	c := l.state.coordinator
	c.mu.Lock()
	switch l.state.kind {
	case mutationLease:
		c.activeMutations--
	case maintenanceLease:
		c.maintenanceActive = false
	}
	c.notifyLocked()
	c.mu.Unlock()
	return nil
}

func (c *Coordinator) changedLocked() chan struct{} {
	if c.changed == nil {
		c.changed = make(chan struct{})
	}
	return c.changed
}

func (c *Coordinator) notifyLocked() {
	changed := c.changedLocked()
	close(changed)
	c.changed = make(chan struct{})
}
