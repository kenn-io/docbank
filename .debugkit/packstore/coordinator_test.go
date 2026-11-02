package packstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorAllowsConcurrentMutationsAndWaitsForAll(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := NewCoordinator()
	first, err := c.AcquireMutation(t.Context())
	require.NoError(err)
	second, err := c.AcquireMutation(t.Context())
	require.NoError(err)

	acquired := make(chan *Lease, 1)
	errCh := make(chan error, 1)
	go func() {
		lease, acquireErr := c.AcquireMaintenance(t.Context())
		if acquireErr != nil {
			errCh <- acquireErr
			return
		}
		acquired <- lease
	}()
	require.Eventually(func() bool { return c.waitingMaintenanceCount() == 1 }, time.Second, time.Millisecond)

	require.NoError(first.Release())
	assert.Never(func() bool { return len(acquired) != 0 }, 20*time.Millisecond, time.Millisecond)
	require.NoError(second.Release())

	select {
	case err := <-errCh:
		require.NoError(err)
	case lease := <-acquired:
		require.NoError(lease.Release())
	case <-time.After(time.Second):
		require.Fail("maintenance did not acquire after every mutation released")
	}
}

func TestCoordinatorGivesQueuedMaintenancePriority(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := NewCoordinator()
	active, err := c.AcquireMutation(t.Context())
	require.NoError(err)

	type leaseResult struct {
		lease *Lease
		err   error
	}
	maintenance := make(chan leaseResult, 1)
	go func() {
		lease, acquireErr := c.AcquireMaintenance(t.Context())
		maintenance <- leaseResult{lease: lease, err: acquireErr}
	}()
	require.Eventually(func() bool { return c.waitingMaintenanceCount() == 1 }, time.Second, time.Millisecond)

	mutation := make(chan leaseResult, 1)
	go func() {
		lease, acquireErr := c.AcquireMutation(t.Context())
		mutation <- leaseResult{lease: lease, err: acquireErr}
	}()
	require.NoError(active.Release())

	var maintenanceLease *Lease
	select {
	case result := <-maintenance:
		require.NoError(result.err)
		maintenanceLease = result.lease
	case <-mutation:
		require.Fail("new mutation bypassed queued maintenance")
	case <-time.After(time.Second):
		require.Fail("neither queued lease acquired")
	}
	assert.Never(func() bool { return len(mutation) != 0 }, 20*time.Millisecond, time.Millisecond)
	require.NoError(maintenanceLease.Release())

	select {
	case result := <-mutation:
		require.NoError(result.err)
		require.NoError(result.lease.Release())
	case <-time.After(time.Second):
		require.Fail("mutation did not resume after maintenance")
	}
}

func TestCoordinatorCancellationRemovesMaintenanceWaiter(t *testing.T) {
	require := require.New(t)
	c := NewCoordinator()
	active, err := c.AcquireMutation(t.Context())
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, acquireErr := c.AcquireMaintenance(ctx)
		errCh <- acquireErr
	}()
	require.Eventually(func() bool { return c.waitingMaintenanceCount() == 1 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(<-errCh, context.Canceled)
	require.Eventually(func() bool { return c.waitingMaintenanceCount() == 0 }, time.Second, time.Millisecond)

	second, err := c.AcquireMutation(t.Context())
	require.NoError(err, "canceled maintenance must not keep blocking mutations")
	require.NoError(second.Release())
	require.NoError(active.Release())
}

func TestCoordinatorRejectsCanceledAcquireAndDoubleRelease(t *testing.T) {
	require := require.New(t)
	c := NewCoordinator()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := c.AcquireMutation(ctx)
	require.ErrorIs(err, context.Canceled)
	_, err = c.AcquireMaintenance(ctx)
	require.ErrorIs(err, context.Canceled)

	lease, err := c.AcquireMutation(t.Context())
	require.NoError(err)
	require.NoError(lease.Release())
	require.ErrorIs(lease.Release(), ErrLeaseReleased)
	copied := *lease
	require.ErrorIs(copied.Release(), ErrLeaseReleased, "copied leases share release state")
}

func TestCoordinatorSupportsIngestThenAutomaticMaintenance(t *testing.T) {
	require := require.New(t)
	c := NewCoordinator()
	ingest, err := c.AcquireMutation(t.Context())
	require.NoError(err)
	require.NoError(ingest.Release())

	maintenance, err := c.AcquireMaintenance(t.Context())
	require.NoError(err)
	require.NoError(maintenance.Release())
}

func TestLeaseValidateAcceptsOnlyLiveLeases(t *testing.T) {
	require := require.New(t)
	c := NewCoordinator()
	live, err := c.AcquireMutation(t.Context())
	require.NoError(err)

	require.NoError(live.Validate())
	require.ErrorIs((*Lease)(nil).Validate(), ErrLeaseReleased)
	require.ErrorIs((&Lease{}).Validate(), ErrLeaseReleased)
	require.ErrorIs((&Lease{state: &leaseState{kind: mutationLease}}).Validate(), ErrLeaseReleased)
	require.ErrorIs((&Lease{state: &leaseState{coordinator: c, kind: leaseKind(255)}}).Validate(), ErrLeaseReleased)
	require.NoError(live.Release())
	require.ErrorIs(live.Validate(), ErrLeaseReleased)
}

func TestLeaseValidateMutationRequiresLiveMutationLease(t *testing.T) {
	require := require.New(t)
	c := NewCoordinator()
	mutation, err := c.AcquireMutation(t.Context())
	require.NoError(err)
	require.NoError(mutation.ValidateMutation())
	require.NoError(mutation.Release())

	maintenance, err := c.AcquireMaintenance(t.Context())
	require.NoError(err)
	require.ErrorIs(maintenance.ValidateMutation(), ErrWrongLeaseKind)
	require.NoError(maintenance.Release())
	require.ErrorIs(maintenance.ValidateMutation(), ErrLeaseReleased)
}

func (c *Coordinator) waitingMaintenanceCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waitingMaintenance
}
