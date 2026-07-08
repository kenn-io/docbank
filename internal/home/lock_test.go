//go:build unix

package home

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockSharedHoldersBlockUpgrade(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	require.NoError(t, l.Ensure())

	a, err := l.AcquireLock(false)
	require.NoError(t, err)
	b, err := l.AcquireLock(false) // shared locks coexist
	require.NoError(t, err)

	ok, err := a.TryUpgrade()
	require.NoError(t, err)
	assert.False(t, ok, "upgrade must fail while another shared holder exists")

	require.NoError(t, b.Release())
	ok, err = a.TryUpgrade()
	require.NoError(t, err)
	assert.True(t, ok, "upgrade must succeed once sole holder")

	require.NoError(t, a.Downgrade())
	c, err := l.AcquireLock(false) // downgraded lock admits shared holders again
	require.NoError(t, err)
	require.NoError(t, c.Release())
	require.NoError(t, a.Release())
}

func TestLockExclusiveBlocksOthers(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	require.NoError(t, l.Ensure())

	ex, err := l.AcquireLock(true)
	require.NoError(t, err)

	acquired := make(chan struct{})
	go func() {
		defer close(acquired)
		sh, err := l.AcquireLock(false)
		assert.NoError(t, err)
		if err == nil {
			assert.NoError(t, sh.Release())
		}
	}()

	select {
	case <-acquired:
		require.Fail(t, "shared acquire must block while exclusive is held")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, ex.Release())
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		require.Fail(t, "shared acquire must proceed after exclusive release")
	}
}

func TestTryLockExclusive(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	require.NoError(t, l.Ensure())

	lk, err := l.TryLockExclusive()
	require.NoError(t, err)
	_, err = l.TryLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, lk.Release())

	lk2, err := l.TryLockExclusive()
	require.NoError(t, err)
	require.NoError(t, lk2.Release())
}

// flock lock conversions are release-then-acquire, so a failed TryUpgrade
// could silently drop the shared lock; the holder must still be counted
// afterwards or gc could run concurrently with a command that believes it
// holds the vault.
func TestFailedUpgradeRetainsSharedLock(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	require.NoError(t, l.Ensure())

	a, err := l.AcquireLock(false)
	require.NoError(t, err)
	b, err := l.AcquireLock(false)
	require.NoError(t, err)

	ok, err := a.TryUpgrade() // fails: b also holds shared
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, b.Release())

	// If a's shared lock survived, a third holder cannot upgrade.
	c, err := l.AcquireLock(false)
	require.NoError(t, err)
	ok, err = c.TryUpgrade()
	require.NoError(t, err)
	assert.False(t, ok, "a's shared lock must survive its failed upgrade")

	require.NoError(t, c.Release())
	require.NoError(t, a.Release())
}
