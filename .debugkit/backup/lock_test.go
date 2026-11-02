package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func initTestRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	return r
}

func TestExclusiveLockRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)

	data, err := os.ReadFile(r.Path("locks", "exclusive.json"))
	require.NoError(err)
	var info LockInfo
	require.NoError(json.Unmarshal(data, &info))
	assert.Equal("create", info.Operation)
	assert.Equal(os.Getpid(), info.PID)
	assert.NotEmpty(info.Hostname)

	require.NoError(l.Release())
	_, statErr := os.Stat(r.Path("locks", "exclusive.json"))
	assert.True(os.IsNotExist(statErr))
}

func TestExclusiveLockConflicts(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	defer func() { require.NoError(l.Release()) }()

	_, err = r.AcquireExclusiveLock("prune", false)
	require.ErrorIs(err, ErrRepoLocked)
	_, err = r.AcquireSharedLock("verify", false)
	require.ErrorIs(err, ErrRepoLocked)
}

func TestSharedLocksCoexist(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	a, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	b, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	require.NoError(a.Release())
	require.NoError(b.Release())
}

func TestExclusiveWaitsOutSharedLocks(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	shared, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)

	oldTimeout, oldPoll := sharedWaitTimeout, sharedWaitPoll
	sharedWaitTimeout, sharedWaitPoll = 2*time.Second, 20*time.Millisecond
	t.Cleanup(func() { sharedWaitTimeout, sharedWaitPoll = oldTimeout, oldPoll })

	done := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond) //nolint:kennlint // sleeptest: exercises a real wait on the shared lock
		done <- shared.Release()
	}()

	excl, err := r.AcquireExclusiveLock("create", false)
	// A failed release is the cause when the wait times out, so report it first.
	require.NoError(<-done)
	require.NoError(err)
	require.NoError(excl.Release())

	// Mirrors the leave-no-file-behind check in
	// TestExclusiveRefusesUnderPersistentShared: once the wait resolves and
	// both locks are released, the locks dir must be clean.
	entries, err := os.ReadDir(r.Path("locks"))
	require.NoError(err)
	assert := assert.New(t)
	assert.Empty(entries)
}

func TestExclusiveRefusesUnderPersistentShared(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	shared, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	defer func() { require.NoError(shared.Release()) }()

	oldTimeout, oldPoll := sharedWaitTimeout, sharedWaitPoll
	sharedWaitTimeout, sharedWaitPoll = 300*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { sharedWaitTimeout, sharedWaitPoll = oldTimeout, oldPoll })

	_, err = r.AcquireExclusiveLock("create", false)
	require.ErrorIs(err, ErrRepoLocked)
	// The failed exclusive attempt must not leave its lock file behind.
	_, statErr := os.Stat(r.Path("locks", "exclusive.json"))
	require.True(os.IsNotExist(statErr))
}

func TestStaleLockIsRemoved(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	l.stopHeartbeat()
	stale := time.Now().Add(-lockStaleAfter - time.Minute)
	require.NoError(os.Chtimes(r.Path("locks", "exclusive.json"), stale, stale))

	l2, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	require.NoError(l2.Release())
}

func TestForceOverridesFreshLock(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	l.stopHeartbeat()

	l2, err := r.AcquireExclusiveLock("create", true)
	require.NoError(err)
	require.NoError(l2.Release())
}

func TestHeartbeatTouchesLockFile(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	oldInterval := lockHeartbeatInterval
	lockHeartbeatInterval = 30 * time.Millisecond
	t.Cleanup(func() { lockHeartbeatInterval = oldInterval })

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	defer func() { require.NoError(l.Release()) }()

	past := time.Now().Add(-time.Hour)
	require.NoError(os.Chtimes(r.Path("locks", "exclusive.json"), past, past))
	require.Eventually(func() bool {
		info, statErr := os.Stat(r.Path("locks", "exclusive.json"))
		return statErr == nil && time.Since(info.ModTime()) < time.Minute
	}, 2*time.Second, 10*time.Millisecond)
}

// TestHeartbeatStopsAfterReplant pins the fix for the heartbeat goroutine
// refreshing a lock file's mtime unconditionally. If this holder is reaped
// as stale while still running and a successor replants the same path with
// its own LockInfo, the original holder's heartbeat must detect the mismatch
// on its next tick and stop touching the file, rather than keeping the
// successor's lock artificially fresh forever.
func TestHeartbeatStopsAfterReplant(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	oldInterval := lockHeartbeatInterval
	lockHeartbeatInterval = 30 * time.Millisecond
	t.Cleanup(func() { lockHeartbeatInterval = oldInterval })

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	t.Cleanup(func() { _ = os.Remove(l.path) })

	// Simulate a stale reap-and-replant: a successor's LockInfo now occupies
	// l.path even though l's heartbeat goroutine is still running.
	successor := LockInfo{
		Hostname:   "successor-host",
		PID:        99998,
		Operation:  "prune",
		AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(successor)
	require.NoError(err)
	require.NoError(os.WriteFile(l.path, body, 0o600))
	past := time.Now().Add(-time.Hour)
	require.NoError(os.Chtimes(l.path, past, past))

	// Give the heartbeat goroutine several ticks' worth of time to observe
	// the mismatch and stop.
	time.Sleep(10 * lockHeartbeatInterval) //nolint:kennlint // sleeptest: heartbeat goroutine runs on the wall clock

	info, statErr := os.Stat(l.path)
	require.NoError(statErr)
	require.Equal(past.Unix(), info.ModTime().Unix(),
		"heartbeat must not refresh a replanted lock's mtime")

	l.stopHeartbeat()
}

// TestSharedLockPostCreateRecheckDetectsExclusive pins the create-then-verify
// handshake that closes the TOCTOU race between AcquireSharedLock's pre-plant
// check and AcquireExclusiveLock's plant. There is no exclusive.json when
// AcquireSharedLock starts, so the pre-plant check passes; sharedLockPostPlantHook
// then hand-plants a fresh exclusive.json, standing in for an exclusive
// acquirer that finished planting and scanning in the window between the
// shared acquirer's pre-check and its own plant. Without the post-create
// re-check, AcquireSharedLock would succeed and leave its shared-<ulid>.json
// file behind even though a fresh exclusive lock now exists; the fix must
// refuse and clean up after itself, leaving the hand-planted exclusive lock
// in place.
func TestSharedLockPostCreateRecheckDetectsExclusive(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	oldHook := sharedLockPostPlantHook
	t.Cleanup(func() { sharedLockPostPlantHook = oldHook })
	sharedLockPostPlantHook = func() {
		info := LockInfo{
			Hostname:   "racer-host",
			PID:        99999,
			Operation:  "create",
			AcquiredAt: time.Now().UTC().Format(time.RFC3339),
		}
		body, err := json.Marshal(info)
		require.NoError(err)
		require.NoError(os.WriteFile(r.Path("locks", "exclusive.json"), body, 0o600))
	}

	_, err := r.AcquireSharedLock("verify", false)
	require.ErrorIs(err, ErrRepoLocked)

	entries, err := os.ReadDir(r.Path("locks"))
	require.NoError(err)
	for _, e := range entries {
		require.False(strings.HasPrefix(e.Name(), "shared-"),
			"shared lock file left behind: %s", e.Name())
	}

	_, statErr := os.Stat(r.Path("locks", "exclusive.json"))
	require.NoError(statErr, "hand-planted exclusive lock should still exist")
}

// TestReleaseDoesNotRemoveReplantedLock pins the fix for Release() removing
// whatever is at its path unconditionally. If holder A's lock is reaped as
// stale (A was slow, not dead) and holder B replants exclusive.json at the
// same path, A's later Release must not delete B's live lock.
func TestReleaseDoesNotRemoveReplantedLock(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	a, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	a.stopHeartbeat()
	stale := time.Now().Add(-lockStaleAfter - time.Minute)
	require.NoError(os.Chtimes(r.Path("locks", "exclusive.json"), stale, stale))

	b, err := r.AcquireExclusiveLock("prune", false)
	require.NoError(err)

	require.NoError(a.Release())

	_, statErr := os.Stat(r.Path("locks", "exclusive.json"))
	require.NoError(statErr, "B's lock file should still exist after A.Release()")

	require.NoError(b.Release())
}

// TestReleaseLeavesForeignLockUntouched pins Release's pre-read guard. When
// the lock file at the holder's path carries a different holder's LockInfo
// (this stale holder was reaped and a successor replanted), Release must
// detect the foreign body up front and return without claiming — leaving the
// live successor's lock intact and creating no releasing-*.json claim file, so
// no third acquirer can plant into a rename-away vacancy. The interleaving is
// simulated directly on the filesystem by overwriting the lock body between
// acquire and release.
func TestReleaseLeavesForeignLockUntouched(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	l.stopHeartbeat()

	successor := LockInfo{
		Hostname:   "successor-host",
		PID:        4242,
		Operation:  "prune",
		AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(successor)
	require.NoError(err)
	require.NoError(os.WriteFile(l.path, body, 0o600))

	require.NoError(l.Release(), "releasing a foreign lock is not an error for us")

	data, statErr := os.ReadFile(l.path)
	require.NoError(statErr, "the successor's lock must survive our release")
	var got LockInfo
	require.NoError(json.Unmarshal(data, &got))
	assert.Equal(t, successor, got)

	entries, err := os.ReadDir(r.Path("locks"))
	require.NoError(err)
	for _, e := range entries {
		require.False(strings.HasPrefix(e.Name(), "releasing-"),
			"stray claim file left behind: %s", e.Name())
	}
	require.NoError(os.Remove(l.path))
}

// TestAcquireReapsStaleReleasingClaims pins the sweep of orphaned
// releasing-*.json claim files: a crash between claimLockFile and the matching
// remove/restore would otherwise leak one forever. Acquisition removes stale
// claims but leaves fresh ones (an in-progress reap/release) alone.
func TestAcquireReapsStaleReleasingClaims(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	staleClaim := r.Path("locks", "releasing-stale.json")
	freshClaim := r.Path("locks", "releasing-fresh.json")
	require.NoError(os.WriteFile(staleClaim, []byte("{}"), 0o600))
	require.NoError(os.WriteFile(freshClaim, []byte("{}"), 0o600))
	stale := time.Now().Add(-lockStaleAfter - time.Minute)
	require.NoError(os.Chtimes(staleClaim, stale, stale))

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	defer func() { require.NoError(l.Release()) }()

	_, statErr := os.Stat(staleClaim)
	require.True(os.IsNotExist(statErr), "a stale claim file must be reaped on acquisition")
	_, statErr = os.Stat(freshClaim)
	require.NoError(statErr, "a fresh claim file must survive acquisition")
}

// TestReleaseSurfacesUnverifiableLockError pins the distinction in Release's
// ownership re-read: os.ErrNotExist means the lock is already gone (not our
// error), but a lock that can neither be verified as ours nor restored must
// surface an error instead of being swallowed. The lock path is made a
// directory, which fails both the ownership re-read (EISDIR) and the
// copy-based restore, so Release must report a non-ErrNotExist error.
func TestReleaseSurfacesUnverifiableLockError(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	l.stopHeartbeat()

	require.NoError(os.Remove(l.path))
	require.NoError(os.Mkdir(l.path, 0o700))
	t.Cleanup(func() { _ = os.RemoveAll(r.Path("locks")) })

	err = l.Release()
	require.Error(err)
	require.NotErrorIs(err, os.ErrNotExist)
}

// TestReturnClaimedLockRefusesSymlinkClaim pins the lock-restore exposure: a
// locks/ writer can plant a symlink as a lock file, and the claim rename
// preserves it. Restoring it would follow the link — link(2) follows the
// source on some platforms, and the copy fallback reads through it — copying
// an arbitrary readable host file into the repository under a lock file's
// name. The claim must be refused and removed instead.
func TestReturnClaimedLockRefusesSymlinkClaim(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	require.NoError(os.WriteFile(secret, []byte("private key bytes"), 0o600))
	path := filepath.Join(dir, exclusiveLockName)
	claim := filepath.Join(dir, releasingClaimPrefix+"symlink.json")
	if err := os.Symlink(secret, claim); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	restored, err := returnClaimedLock(path, claim)
	require.ErrorContains(err, "not a regular file")
	require.False(restored)
	_, statErr := os.Lstat(claim)
	require.ErrorIs(statErr, os.ErrNotExist, "the symlink debris must be removed")
	_, statErr = os.Stat(path)
	require.ErrorIs(statErr, os.ErrNotExist, "nothing must be planted at the lock path from a symlink claim")
}

// TestRestoreClaimByCopyRefusesReplacedClaim pins the copy fallback's identity
// check: the descriptor it reads must be the same file returnClaimedLock's
// regular-file check lstated, so a claim swapped between the two (for a
// symlink, say) is refused rather than read through.
func TestRestoreClaimByCopyRefusesReplacedClaim(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, exclusiveLockName)
	claim := filepath.Join(dir, releasingClaimPrefix+"swapped.json")
	require.NoError(os.WriteFile(claim, []byte(`{"pid":1}`), 0o600))
	other := filepath.Join(dir, "other.json")
	require.NoError(os.WriteFile(other, []byte(`{"pid":2}`), 0o600))
	otherInfo, err := os.Lstat(other)
	require.NoError(err)

	restored, err := restoreClaimByCopy(path, claim, otherInfo)
	require.ErrorContains(err, "replaced during restore")
	require.False(restored)
	_, statErr := os.Stat(path)
	require.ErrorIs(statErr, os.ErrNotExist, "nothing must be planted from an unverified claim")
}

// TestReturnClaimedLockCopyFallbackRestores pins the no-hardlink fallback: when
// osLink is unavailable (exFAT/FAT32/SMB/NFS), returnClaimedLock must reproduce
// the claimed lock at its path by copying, not by a clobbering rename.
func TestReturnClaimedLockCopyFallbackRestores(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, exclusiveLockName)
	claim := filepath.Join(dir, releasingClaimPrefix+"restore.json")
	body := []byte(`{"pid":1234,"operation":"create"}`)
	require.NoError(os.WriteFile(claim, body, 0o600))

	old := osLink
	osLink = func(string, string) error { return errors.New("no hard links on this filesystem") }
	t.Cleanup(func() { osLink = old })

	restored, err := returnClaimedLock(path, claim)
	require.NoError(err)
	require.True(restored)

	got, err := os.ReadFile(path)
	require.NoError(err)
	require.Equal(body, got, "the claimed lock's contents must be restored at path via the copy fallback")
	_, statErr := os.Stat(claim)
	require.True(os.IsNotExist(statErr), "the claim must be removed after a successful restore")
}

// TestReturnClaimedLockCopyFallbackDoesNotClobber pins the mutual-exclusion
// guarantee the copy fallback restores: a lock planted at the path during the
// claim window must survive, matching the os.Link-EEXIST branch. The old rename
// fallback would have overwritten it.
func TestReturnClaimedLockCopyFallbackDoesNotClobber(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, exclusiveLockName)
	claim := filepath.Join(dir, releasingClaimPrefix+"clobber.json")
	require.NoError(os.WriteFile(claim, []byte("stale claimed copy"), 0o600))
	live := []byte(`{"pid":9999,"operation":"prune"}`)
	require.NoError(os.WriteFile(path, live, 0o600))

	old := osLink
	osLink = func(string, string) error { return errors.New("no hard links on this filesystem") }
	t.Cleanup(func() { osLink = old })

	restored, err := returnClaimedLock(path, claim)
	require.NoError(err)
	require.False(restored, "a lock already at path is a live replacement, not our restore")

	got, err := os.ReadFile(path)
	require.NoError(err)
	require.Equal(live, got, "the copy fallback must never clobber a lock planted during the claim window")
	_, statErr := os.Stat(claim)
	require.True(os.IsNotExist(statErr), "the stale claim must be dropped, not left behind")
}
