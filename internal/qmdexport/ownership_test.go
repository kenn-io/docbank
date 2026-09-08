package qmdexport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func syntheticPrivateBase(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, safefileio.EnsurePrivateDir(base))
	return base
}

func readSyntheticMarker(t *testing.T, target string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(target, ".docbank-qmd-export.json"))
	require.NoError(t, err)
	return data
}

func readSyntheticSentinel(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// Every refusal fixture retains an unrelated private file, including its mode.
func syntheticSentinel(t *testing.T, base string) {
	t.Helper()
	path := filepath.Join(base, "unrelated")
	require.NoError(t, os.WriteFile(path, []byte("synthetic sentinel\n"), 0o600))
	before, err := os.Stat(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.Equal(t, []byte("synthetic sentinel\n"), readSyntheticSentinel(t, path))
		after, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, before.Mode(), after.Mode())
	})
}

func TestOwnershipClaimAndStablePublishLock(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	target := filepath.Join(base, "export")
	first, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	marker := readSyntheticMarker(t, target)
	lockBefore, err := os.Stat(filepath.Join(target, ".publish.lock"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	waiting := make(chan struct{})
	done := make(chan error, 1)
	t.Cleanup(cancel)
	go func() {
		second, err := acquireOwnedRoot(ctx, target, ownershipHooks{waitingOnLock: func() { close(waiting) }})
		if second != nil {
			err = second.Close()
		}
		done <- err
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("publisher did not contend on the stable lock")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("publisher ignored cancellation")
	}
	require.Equal(t, marker, readSyntheticMarker(t, target))
	require.NoError(t, first.Close())
	second, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.NoError(t, err)
	require.NoError(t, second.Close())
	require.Equal(t, marker, readSyntheticMarker(t, target))
	lockAfter, err := os.Stat(filepath.Join(target, ".publish.lock"))
	require.NoError(t, err)
	require.True(t, os.SameFile(lockBefore, lockAfter))
}

func TestOwnershipReadOnlyDoesNotClaim(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	target := filepath.Join(base, "export")
	require.NoError(t, preflightRoot(t.Context(), target))
	_, err := os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = openOwnedRoot(t.Context(), target)
	require.Error(t, err)
	_, err = os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist)
	var nilContext context.Context
	require.Error(t, preflightRoot(nilContext, target))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, preflightRoot(ctx, target), context.Canceled)
	require.Error(t, preflightRoot(t.Context(), "relative"))
	require.Error(t, preflightRoot(t.Context(), filepath.VolumeName(base)+string(filepath.Separator)))
}

func TestOwnershipRefusesIncompleteReservedLayout(t *testing.T) {
	for _, kind := range []string{"missing-lock", "missing-marker", "corrupt-marker", "unknown-member", "oversize-marker", "invalid-current", "extra-current-byte", "missing-generations"} {
		t.Run(kind, func(t *testing.T) {
			base := syntheticPrivateBase(t)
			syntheticSentinel(t, base)
			target := filepath.Join(base, "export")
			r, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
			require.NoError(t, err)
			require.NoError(t, r.Close())
			switch kind {
			case "missing-lock":
				require.NoError(t, os.Remove(filepath.Join(target, lockName)))
			case "missing-marker":
				require.NoError(t, os.Remove(filepath.Join(target, markerName)))
			case "corrupt-marker":
				require.NoError(t, os.WriteFile(filepath.Join(target, markerName), []byte("{broken"), 0o600))
			case "unknown-member":
				require.NoError(t, os.WriteFile(filepath.Join(target, markerName), []byte(`{"format":"docbank-qmd-export-root","version":1,"id":"0123456789abcdef0123456789abcdef","unknown":true}`), 0o600))
			case "oversize-marker":
				require.NoError(t, os.WriteFile(filepath.Join(target, markerName), make([]byte, 4097), 0o600))
			case "invalid-current":
				require.NoError(t, os.WriteFile(filepath.Join(target, "CURRENT"), []byte("wrong\n"), 0o600))
			case "extra-current-byte":
				require.NoError(t, os.WriteFile(filepath.Join(target, "CURRENT"), []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n\n"), 0o600))
			case "missing-generations":
				require.NoError(t, os.Remove(filepath.Join(target, "generations")))
			}
			before, err := os.ReadDir(target)
			require.NoError(t, err)
			require.Error(t, preflightRoot(t.Context(), target))
			_, err = openOwnedRoot(t.Context(), target)
			require.Error(t, err)
			_, err = acquireOwnedRoot(t.Context(), target, ownershipHooks{})
			require.Error(t, err)
			after, err := os.ReadDir(target)
			require.NoError(t, err)
			require.Len(t, after, len(before))
		})
	}
}

func TestOwnershipRootEnumerationBound(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	target := filepath.Join(base, "export")
	r, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.NoError(t, err)
	for i := range 1021 {
		f, _, err := r.root.createFile(fmt.Sprintf("unknown-%04d", i), movableEntry)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	require.NoError(t, r.Close())
	require.ErrorIs(t, preflightRoot(t.Context(), target), errDirectoryBound)
}

func TestOwnershipBootstrapFailurePreservesLockForContender(t *testing.T) {
	for _, phase := range []string{"before-marker", "after-marker"} {
		t.Run(phase, func(t *testing.T) {
			base := syntheticPrivateBase(t)
			syntheticSentinel(t, base)
			target := filepath.Join(base, "export")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			creatorCtx, creatorCancel := context.WithCancel(ctx)
			reached := make(chan struct{})
			release := make(chan struct{})
			creatorDone := make(chan error, 1)
			waiterDone := make(chan error, 1)
			waiting := make(chan struct{})
			t.Cleanup(cancel)
			t.Cleanup(creatorCancel)
			pause := func() {
				close(reached)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			hooks := ownershipHooks{}
			if phase == "before-marker" {
				hooks.afterBootstrapLock = pause
			} else {
				hooks.afterMarker = pause
			}
			go func() {
				r, err := acquireOwnedRoot(creatorCtx, target, hooks)
				if r != nil {
					_ = r.Close()
				}
				creatorDone <- err
			}()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal("creator did not reach bootstrap checkpoint")
			}
			lockBefore, err := os.Stat(filepath.Join(target, lockName))
			require.NoError(t, err)
			if phase == "before-marker" {
				require.NoError(t, preflightRoot(ctx, target))
			}
			go func() {
				r, err := acquireOwnedRoot(ctx, target, ownershipHooks{waitingOnLock: func() { close(waiting) }})
				if r != nil {
					_ = r.Close()
				}
				waiterDone <- err
			}()
			select {
			case <-waiting:
			case <-ctx.Done():
				t.Fatal("contender did not wait")
			}
			creatorCancel()
			close(release)
			select {
			case err := <-creatorDone:
				require.ErrorIs(t, err, context.Canceled)
			case <-ctx.Done():
				t.Fatal("creator did not stop")
			}
			select {
			case err := <-waiterDone:
				require.Error(t, err)
			case <-ctx.Done():
				t.Fatal("contender did not stop")
			}
			lockAfter, err := os.Stat(filepath.Join(target, lockName))
			require.NoError(t, err)
			require.True(t, os.SameFile(lockBefore, lockAfter))
			entries, err := os.ReadDir(target)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, lockName, entries[0].Name())
			_, err = openOwnedRoot(ctx, target)
			require.Error(t, err)
			_, err = acquireOwnedRoot(ctx, target, ownershipHooks{})
			require.Error(t, err)
		})
	}
}

func TestOwnershipClaimPrivateEmptyRootAndReadOnlyOpen(t *testing.T) {
	base := syntheticPrivateBase(t)
	target := filepath.Join(base, "export")
	require.NoError(t, safefileio.EnsurePrivateDir(target))
	r, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	before := readSyntheticMarker(t, target)
	readOnly, err := openOwnedRoot(t.Context(), target)
	require.NoError(t, err)
	require.NoError(t, readOnly.Close())
	require.Equal(t, before, readSyntheticMarker(t, target))
	d, id, err := r.staging.createDir("empty", movableEntry)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	require.NoError(t, r.staging.removeEntry("empty", id, true))
}

func TestOwnershipClaimRecheckPreservesUnknownEntry(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	target := filepath.Join(base, "export")
	_, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{beforeClaimRecheck: func() {
		require.NoError(t, os.WriteFile(filepath.Join(target, "unknown"), []byte("untouched"), 0o600))
	}})
	require.Error(t, err)
	require.Equal(t, []byte("untouched"), readSyntheticSentinel(t, filepath.Join(target, "unknown")))
	_, err = os.Stat(filepath.Join(target, markerName))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestOwnershipMarkerSubstitutionPreservedOnRollback(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	target := filepath.Join(base, "export")
	r, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{afterMarker: func() {
		require.NoError(t, os.Rename(filepath.Join(target, markerName), filepath.Join(target, "original-marker")))
		require.NoError(t, os.WriteFile(filepath.Join(target, markerName), []byte("unknown replacement"), 0o600))
	}})
	if r != nil {
		require.NoError(t, r.Close())
	}
	require.Error(t, err)
	require.Equal(t, []byte("unknown replacement"), readSyntheticMarker(t, target))
	_, err = os.Stat(filepath.Join(target, "generations"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
