package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/internal/renamebusy"
	"go.kenn.io/kit/pack"
)

// ErrRepoLocked reports that another operation holds a conflicting repo lock.
var ErrRepoLocked = errors.New("backup: repository is locked")

var (
	lockStaleAfter        = 30 * time.Minute
	lockHeartbeatInterval = 30 * time.Second
	sharedWaitTimeout     = 60 * time.Second
	sharedWaitPoll        = 200 * time.Millisecond
	claimBusyTimeout      = renamebusy.DefaultTimeout
)

const exclusiveLockName = "exclusive.json"

// releasingClaimPrefix names the transient files claimLockFile renames a lock
// to while a reaper or Release inspects it. A crash between claim and
// remove/restore leaves one behind; reapStaleClaims sweeps stale ones.
const releasingClaimPrefix = "releasing-"

// sharedLockPostPlantHook runs between planting a shared lock file and the
// exclusive re-check; tests use it to open the race window deterministically.
var sharedLockPostPlantHook = func() {}

// osLink is the hard-link primitive returnClaimedLock uses to restore a claimed
// lock without clobbering. It is a var so tests can simulate the filesystems
// (exFAT, FAT32, many SMB/NFS mounts) where os.Link is unavailable and the
// no-clobber copy fallback must take over.
var osLink = os.Link

// LockInfo is the JSON body of a repo lock file. Freshness is carried by the
// file's mtime (heartbeat), not by fields, so observers need no clock sync.
type LockInfo struct {
	Hostname   string `json:"hostname"`
	PID        int    `json:"pid"`
	Operation  string `json:"operation"`
	AcquiredAt string `json:"acquired_at"`
}

// RepoLock is a held repository lock with a heartbeat goroutine. info holds
// the exact LockInfo this process wrote, so Release can verify it still owns
// the file at path before removing it (the file may have been reaped as
// stale and replanted by another holder in the meantime).
type RepoLock struct {
	path string
	info LockInfo
	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// AcquireExclusiveLock takes locks/exclusive.json for a mutating operation.
// It removes stale locks, refuses fresh ones unless force is set, and after
// planting the exclusive file waits out fresh shared locks (releasing and
// failing if they persist past sharedWaitTimeout).
func (r *Repo) AcquireExclusiveLock(operation string, force bool) (*RepoLock, error) {
	return r.AcquireExclusiveLockContext(context.Background(), operation, force)
}

// AcquireExclusiveLockContext is AcquireExclusiveLock with a cancellable wait
// for active readers. Cancellation releases this attempt's exclusive claim.
// It does not interrupt an operating-system filesystem call already in progress.
func (r *Repo) AcquireExclusiveLockContext(ctx context.Context, operation string, force bool) (*RepoLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.reapStaleClaims()
	path := r.Path(locksDirName, exclusiveLockName)
	if err := clearConflicting(path, force); err != nil {
		return nil, err
	}
	lock, err := plantLock(path, operation)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(sharedWaitTimeout)
	ticker := time.NewTicker(sharedWaitPoll)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, lock.Release())
		}
		holders, err := r.freshSharedLocks(force)
		if err != nil {
			_ = lock.Release()
			return nil, err
		}
		if len(holders) == 0 {
			return lock, nil
		}
		if time.Now().After(deadline) {
			_ = lock.Release()
			return nil, fmt.Errorf(
				"%w: shared lock(s) held by %s",
				ErrRepoLocked,
				strings.Join(holders, ", "),
			)
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), lock.Release())
		case <-ticker.C:
		}
	}
}

// AcquireSharedLock takes locks/shared-<ulid>.json for a read-walking
// operation (verify, restore). It refuses under a fresh exclusive lock.
//
// The pre-plant check alone is racy: AcquireExclusiveLock could plant
// exclusive.json and finish its (single) freshSharedLocks scan in the window
// between our check and our own plant, and both sides would then believe
// they hold a compatible lock. Closing that requires the standard
// create-then-verify handshake: after planting our shared file we re-check
// for a fresh exclusive lock and back off if one is now present. This is
// safe for the mirrored ordering too — if our shared file lands first,
// AcquireExclusiveLock's freshSharedLocks scan (which always runs after its
// own plant) will see it and wait.
func (r *Repo) AcquireSharedLock(operation string, force bool) (*RepoLock, error) {
	r.reapStaleClaims()
	exclusive := r.Path(locksDirName, exclusiveLockName)
	if err := clearConflicting(exclusive, force); err != nil {
		return nil, err
	}
	name := "shared-" + pack.NewPackID() + ".json"
	lock, err := plantLock(r.Path(locksDirName, name), operation)
	if err != nil {
		return nil, err
	}
	sharedLockPostPlantHook()
	if err := clearConflicting(exclusive, force); err != nil {
		_ = lock.Release()
		return nil, err
	}
	return lock, nil
}

// lockIsFresh reports whether a lock file's mtime is recent enough that its
// holder is presumed alive (refreshed within lockStaleAfter of now). Every
// reaper judges staleness through this one helper so the threshold stays
// consistent.
func lockIsFresh(info os.FileInfo) bool {
	return time.Since(info.ModTime()) <= lockStaleAfter
}

// reapStaleClaims removes orphaned releasing-*.json claim files left by a
// crash between claimLockFile and the matching remove/restore. Fresh claims
// belong to an in-progress reap or release and are left alone. Best-effort:
// a failed scan or remove leaves the sweep for a later acquisition.
func (r *Repo) reapStaleClaims() {
	entries, err := os.ReadDir(r.Path(locksDirName))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), releasingClaimPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished between readdir and stat
		}
		if !lockIsFresh(info) {
			_ = os.Remove(r.Path(locksDirName, e.Name()))
		}
	}
}

// clearConflicting removes path if it is stale (or force is set); it returns
// ErrRepoLocked if a fresh lock remains.
func clearConflicting(path string, force bool) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(
			"backup: checking lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	if force || !lockIsFresh(info) {
		removed, err := reapLock(path, force)
		if err != nil {
			return err
		}
		if removed {
			return nil
		}
		// A live holder refreshed or replanted between our staleness check and
		// the claim: fall through and report the repository as locked.
	}
	return fmt.Errorf(
		"%w: %s held by %s",
		ErrRepoLocked,
		filepath.Base(path),
		describeLock(path),
	)
}

func (r *Repo) freshSharedLocks(force bool) ([]string, error) {
	entries, err := os.ReadDir(r.Path(locksDirName))
	if err != nil {
		return nil, fmt.Errorf("backup: reading locks dir: %w", err)
	}
	var holders []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "shared-") {
			continue
		}
		path := r.Path(locksDirName, e.Name())
		info, err := e.Info()
		if err != nil {
			continue // lock vanished between readdir and stat
		}
		if force || !lockIsFresh(info) {
			if removed, _ := reapLock(path, force); removed {
				continue
			}
			// Still fresh after the claim: count it as a live holder below.
		}
		holders = append(holders, describeLock(path))
	}
	return holders, nil
}

// claimLockFile atomically claims the lock file at path by renaming it to a
// unique sibling, so at most one caller can win the claim of a given file: the
// winner receives the file, everyone else sees os.ErrNotExist. The returned
// claim path names the file the caller now solely owns. An os.ErrNotExist
// error means the lock was already gone — reaped, released, or claimed by
// someone else — which callers treat as "not ours to act on".
func claimLockFile(path string) (string, error) {
	claim := filepath.Join(
		filepath.Dir(path),
		releasingClaimPrefix+pack.NewPackID()+".json",
	)
	// A reader elsewhere holds the file only for the length of one read, so a
	// busy file is retried briefly instead of leaving a released lock in place.
	if err := renamebusy.Replace(path, claim, claimBusyTimeout); err != nil {
		return "", err
	}
	return claim, nil
}

// returnClaimedLock puts a claimed lock file back at path without clobbering a
// lock planted there since the claim. restored is true when the claimed file
// is back at path, and false only when a newer lock already occupies path (the
// claimed copy is then dropped). Reapers treat both outcomes as "a live lock
// remains"; Release surfaces the false case as an anomaly.
//
// The restore is attempted with os.Link first: link is atomic and refuses to
// overwrite an existing path, so a lock planted at path during the claim window
// is preserved (EEXIST -> drop the now-stale claim). But os.Link is unavailable
// or unreliable on exFAT, FAT32, and many SMB/NFS mounts — exactly where backup
// repositories often live — so any other link failure falls back to
// restoreClaimByCopy, which reproduces link's no-clobber semantics with an
// O_EXCL create so the fallback can never overwrite a lock planted during the
// claim window either.
func returnClaimedLock(path, claim string) (restored bool, err error) {
	// plantLock only ever creates plain regular files, so anything else at the
	// claim is foreign debris from another locks/ writer — most dangerously a
	// symlink planted as a lock file, which the claim rename preserves. It must
	// not be restored: link(2) follows a symlink source on some platforms
	// (macOS), and the copy fallback reads through one, either of which would
	// pull an arbitrary readable host file into the repository under a lock
	// file's name. Refuse and remove the debris instead.
	claimed, err := os.Lstat(claim)
	if err != nil {
		return false, fmt.Errorf(
			"backup: checking claimed lock %s: %w", filepath.Base(path), err)
	}
	if !claimed.Mode().IsRegular() {
		_ = os.Remove(claim)
		return false, fmt.Errorf(
			"backup: claimed lock %s was not a regular file; removed it instead of restoring",
			filepath.Base(path))
	}
	linkErr := osLink(claim, path)
	if linkErr == nil {
		if err := os.Remove(claim); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf(
				"backup: cleaning up claimed lock %s: %w",
				filepath.Base(path),
				err,
			)
		}
		return true, nil
	}
	if errors.Is(linkErr, os.ErrExist) {
		// A newer lock was planted at path during the claim window; the claimed
		// copy is stale and must go rather than overwrite the live lock.
		_ = os.Remove(claim)
		return false, nil
	}
	// Link is unsupported or unreliable on this filesystem: fall back to a
	// no-clobber copy so a live holder's lock is restored without ever
	// overwriting a lock planted at path during the claim window.
	return restoreClaimByCopy(path, claim, claimed)
}

// restoreClaimByCopy restores a claimed lock file's contents to path when
// os.Link is unavailable, matching the link path's mutual-exclusion semantics:
// the destination is created with O_CREATE|O_EXCL so a lock planted at path
// during the claim window is never clobbered. On EEXIST the existing file is a
// live replacement lock, so — exactly as the link-EEXIST branch does — the
// stale claim is dropped and restored is false. On success the copied bytes are
// fsynced before the claim is removed.
//
// claimed is the claim's Lstat result from returnClaimedLock's regular-file
// check; the descriptor actually read is tied to it with SameFile so a symlink
// swapped in at the claim after that check is refused rather than followed —
// an os.ReadFile here would copy whatever readable file the link points at
// into the repository.
func restoreClaimByCopy(path, claim string, claimed os.FileInfo) (restored bool, err error) {
	data, err := readClaimForRestore(path, claim, claimed)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// A newer lock occupies path (same as the link-EEXIST case): drop the
			// now-stale claim rather than overwrite the live lock.
			_ = os.Remove(claim)
			return false, nil
		}
		return false, fmt.Errorf(
			"backup: restoring claimed lock %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return false, fmt.Errorf(
			"backup: writing restored lock %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return false, fmt.Errorf(
			"backup: syncing restored lock %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf(
			"backup: closing restored lock %s: %w", filepath.Base(path), err)
	}
	if err := os.Remove(claim); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf(
			"backup: cleaning up claimed lock %s: %w", filepath.Base(path), err)
	}
	return true, nil
}

// readClaimForRestore reads the claimed lock's bytes through a descriptor
// tied by SameFile to the Lstat identity returnClaimedLock verified. The
// handle is closed before this returns — not deferred to the caller's scope —
// because Windows refuses to remove a file that is still open, and every path
// out of restoreClaimByCopy removes the claim.
func readClaimForRestore(path, claim string, claimed os.FileInfo) ([]byte, error) {
	cf, err := os.Open(claim)
	if err != nil {
		return nil, fmt.Errorf(
			"backup: opening claimed lock %s for restore: %w",
			filepath.Base(path), err)
	}
	defer func() { _ = cf.Close() }()
	held, err := cf.Stat()
	if err != nil {
		return nil, fmt.Errorf(
			"backup: checking claimed lock %s for restore: %w",
			filepath.Base(path), err)
	}
	if !os.SameFile(claimed, held) {
		return nil, fmt.Errorf(
			"backup: claimed lock %s was replaced during restore",
			filepath.Base(path))
	}
	data, err := io.ReadAll(cf)
	if err != nil {
		return nil, fmt.Errorf(
			"backup: reading claimed lock %s for restore: %w",
			filepath.Base(path), err)
	}
	return data, nil
}

// reapLock removes the lock at path when force is set or it is still stale
// after being claimed, closing the check-then-remove window a plain os.Remove
// leaves open: a lock refreshed by its holder's heartbeat, or reaped and
// replanted by a successor, between the staleness check and the removal would
// otherwise be deleted out from under a live holder. Claiming by rename first
// means the file re-evaluated and removed is the one this call solely owns.
// removed reports whether the conflicting lock is gone afterward.
func reapLock(path string, force bool) (removed bool, err error) {
	claim, err := claimLockFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil // already gone
	}
	if err != nil {
		return false, fmt.Errorf(
			"backup: claiming lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	if !force {
		if info, statErr := os.Stat(claim); statErr == nil &&
			lockIsFresh(info) {
			// Refreshed or replanted since we judged it stale: put it back.
			if _, err := returnClaimedLock(path, claim); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	if err := os.Remove(claim); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf(
			"backup: removing lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	return true, nil
}

func plantLock(path, operation string) (*RepoLock, error) {
	hostname, _ := os.Hostname()
	info := LockInfo{
		Hostname:   hostname,
		PID:        os.Getpid(),
		Operation:  operation,
		AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("backup: encoding lock info: %w", err)
	}
	f, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o600,
	)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf(
				"%w: %s held by %s",
				ErrRepoLocked,
				filepath.Base(path),
				describeLock(path),
			)
		}
		return nil, fmt.Errorf(
			"backup: creating lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf(
			"backup: writing lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf(
			"backup: closing lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	l := &RepoLock{path: path, info: info, stop: make(chan struct{})}
	l.wg.Add(1)
	go l.heartbeat()
	return l, nil
}

func (l *RepoLock) heartbeat() {
	defer l.wg.Done()
	ticker := time.NewTicker(lockHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			// A holder can be reaped as stale (clearConflicting or
			// freshSharedLocks removing it) and replanted by a successor
			// while this goroutine is still running, e.g. a slow or
			// briefly-descheduled process. Refreshing the file's mtime in
			// that case would keep the successor's lock artificially fresh
			// forever. Re-read and compare identity every tick, matching
			// Release's ownership check, and stop for good on any mismatch
			// or read error (including the file having vanished).
			current, ok, err := currentLockInfo(l.path)
			if err != nil || !ok || current != l.info {
				return
			}
			now := time.Now()
			_ = os.Chtimes(l.path, now, now)
		}
	}
}

func (l *RepoLock) stopHeartbeat() {
	l.once.Do(func() { close(l.stop) })
	l.wg.Wait()
}

// currentLockInfo reads and parses the LockInfo currently stored at path.
// ok is false whenever the file cannot be trusted to represent a live lock
// this process still owns: it is missing, unreadable, or fails to parse.
// err is set only when the file exists but could not be read, so a caller
// wanting to distinguish "definitely not ours" from "we don't know" (as
// Release does) can still surface a real I/O failure; a missing file or an
// unparsable body are reported as ok == false, err == nil.
func currentLockInfo(path string) (info LockInfo, ok bool, err error) {
	data, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return LockInfo{}, false, nil
	}
	if readErr != nil {
		return LockInfo{}, false, readErr
	}
	if parseErr := json.Unmarshal(data, &info); parseErr != nil {
		return LockInfo{}, false, nil //nolint:nilerr // unparsable body: reported as ok == false, not an error
	}
	return info, true, nil
}

// Release stops the heartbeat and removes the lock file, but only if the file
// still holds the LockInfo this RepoLock planted. If this holder was slow
// enough to be reaped as stale, another holder may have replanted the same
// path with its own live lock; removing it would delete that lock out from
// under its owner. Removal is made atomic against that race by claiming the
// file with a rename first: only one caller can win the rename, so the file
// this Release inspects and deletes is one it solely owns. When the claimed
// file is not ours it is restored to its path (without clobbering any newer
// lock), and — because our lock is already gone — Release reports no error.
//
// A cheap pre-read confines the claim protocol to the only case that needs it.
// If the file at l.path already, provably, holds a different holder's LockInfo,
// it belongs to a successor and Release returns without claiming: the old
// behavior of never touching a file whose body is not ours. This avoids the
// claim's momentary rename-away vacancy — into which a third acquirer could
// plant a lock that the return step then drops — in the common replanted case.
// Only an ours-looking or unreadable lock proceeds to the claim handshake.
func (l *RepoLock) Release() error {
	l.stopHeartbeat()
	if current, ok, readErr := currentLockInfo(l.path); readErr == nil && ok && current != l.info {
		return nil // the lock at our path belongs to a successor: leave it be
	}
	claim, err := claimLockFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // reaped, released, or claimed elsewhere: our lock is gone
	}
	if err != nil {
		return fmt.Errorf(
			"backup: claiming lock %s for release: %w",
			filepath.Base(l.path),
			err,
		)
	}
	current, ok, readErr := currentLockInfo(claim)
	if readErr == nil && ok && current == l.info {
		if err := os.Remove(claim); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"backup: releasing lock %s: %w",
				filepath.Base(l.path),
				err,
			)
		}
		return nil
	}
	// The claimed file is not provably ours: a successor reaped our stale lock
	// and replanted before we claimed, or its body could not be read. Put it
	// back without clobbering any newer lock rather than delete a live holder's
	// file out from under it.
	restored, err := returnClaimedLock(l.path, claim)
	if err != nil {
		return err
	}
	if readErr != nil {
		return fmt.Errorf(
			"backup: could not verify lock %s for removal: %w",
			filepath.Base(l.path),
			readErr,
		)
	}
	if !restored {
		return fmt.Errorf(
			"backup: lock %s was replanted during release; claimed foreign lock could not be restored",
			filepath.Base(l.path),
		)
	}
	return nil
}

func describeLock(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown holder"
	}
	var info LockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return "unknown holder"
	}
	return fmt.Sprintf(
		"%s pid %d (%s since %s)",
		info.Hostname,
		info.PID,
		info.Operation,
		info.AcquiredAt,
	)
}
