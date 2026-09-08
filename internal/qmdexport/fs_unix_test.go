//go:build linux || darwin

package qmdexport

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestOwnershipUnixRefusesInsecureAndLinkedRoots(t *testing.T) {
	for _, kind := range []string{"insecure", "populated", "root-link", "ancestor-link", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			base := syntheticPrivateBase(t)
			syntheticSentinel(t, base)
			target := filepath.Join(base, "export")
			require.NoError(t, os.Mkdir(target, 0o700))
			switch kind {
			case "insecure":
				require.NoError(t, os.Chmod(target, 0o755))
			case "populated":
				require.NoError(t, os.WriteFile(filepath.Join(target, "unknown"), []byte("keep"), 0o600))
			case "root-link":
				require.NoError(t, os.Rename(target, filepath.Join(base, "real")))
				require.NoError(t, os.Symlink("real", target))
			case "ancestor-link":
				require.NoError(t, os.Symlink("export", filepath.Join(base, "alias")))
				target = filepath.Join(base, "alias", "child")
			case "fifo":
				require.NoError(t, unix.Mkfifo(filepath.Join(target, lockName), 0o600))
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			before, err := os.Lstat(target)
			if kind != "ancestor-link" {
				require.NoError(t, err)
			}
			// A blocking FIFO open must fail the test at the deadline, without a
			// matching writer disguising the missing O_NONBLOCK protection.
			type outcome struct{ preflight, acquire error }
			done := make(chan outcome, 1)
			go func() {
				preflightErr := preflightRoot(ctx, target)
				r, acquireErr := acquireOwnedRoot(ctx, target, ownershipHooks{})
				if r != nil {
					_ = r.Close()
				}
				done <- outcome{preflightErr, acquireErr}
			}()
			select {
			case result := <-done:
				require.Error(t, result.preflight)
				require.Error(t, result.acquire)
			case <-ctx.Done():
				t.Fatal("reserved entry validation blocked")
			}
			if kind != "ancestor-link" {
				after, err := os.Lstat(target)
				require.NoError(t, err)
				require.Equal(t, before.Mode(), after.Mode())
			}
		})
	}
}

func TestAnchoredUnixRootReplacementAndIdentity(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	target := filepath.Join(base, "export")
	r, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	require.NoError(t, os.Rename(target, filepath.Join(base, "moved")))
	require.NoError(t, os.Mkdir(target, 0o700))
	f, id, err := r.root.createFile("created", movableEntry)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, err = os.Stat(filepath.Join(target, "created"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.Rename(filepath.Join(base, "moved", "created"), filepath.Join(base, "moved", "original")))
	require.NoError(t, os.WriteFile(filepath.Join(base, "moved", "created"), []byte("replacement"), 0o600))
	require.Error(t, r.root.removeEntry("created", id, false))
	require.Equal(t, []byte("replacement"), readSyntheticSentinel(t, filepath.Join(base, "moved", "created")))
}

func TestAnchoredUnixRefusesInsecureManagedDirectory(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	r, err := acquireOwnedRoot(t.Context(), filepath.Join(base, "export"), ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	d, _, err := r.staging.createDir("child", movableEntry)
	require.NoError(t, err)
	require.NoError(t, d.file.Chmod(0o755))
	require.NoError(t, d.Close())
	d, _, err = r.staging.openDir("child")
	if d != nil {
		require.NoError(t, d.Close())
	}
	require.Error(t, err)
}

func TestOwnershipUnixDirectoryACLRefusal(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	target := filepath.Join(base, "export")
	require.NoError(t, os.Mkdir(target, 0o700))
	require.NoError(t, preflightRoot(t.Context(), target))
	readACL := addSyntheticDirectoryACL(t, target)
	aclBefore := readACL()
	before, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), before.Mode().Perm())
	require.Error(t, preflightRoot(t.Context(), target))
	_, err = acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.Error(t, err)
	after, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, before.Mode(), after.Mode())
	require.Equal(t, aclBefore, readACL())
}

func addSyntheticDirectoryACL(t *testing.T, target string) func() []byte {
	t.Helper()
	if runtime.GOOS == "linux" {
		// A named-user ACL masked to zero retains 0700. Even ineffective named
		// grants are rejected under Kit's conservative no-access-ACL policy.
		acl := make([]byte, 4+5*8)
		binary.LittleEndian.PutUint32(acl, 2)
		for i, e := range []struct {
			tag, perm uint16
			id        uint32
		}{
			{1, 7, ^uint32(0)}, {2, 4, uint32(os.Getuid() + 1)}, {4, 0, ^uint32(0)}, {16, 0, ^uint32(0)}, {32, 0, ^uint32(0)},
		} {
			offset := 4 + i*8
			binary.LittleEndian.PutUint16(acl[offset:], e.tag)
			binary.LittleEndian.PutUint16(acl[offset+2:], e.perm)
			binary.LittleEndian.PutUint32(acl[offset+4:], e.id)
		}
		f, err := os.Open(target)
		require.NoError(t, err)
		applied := false
		t.Cleanup(func() {
			var removeErr error
			if applied {
				removeErr = unix.Fremovexattr(int(f.Fd()), "system.posix_acl_access")
			}
			require.NoError(t, errors.Join(removeErr, f.Close()))
		})
		err = unix.Fsetxattr(int(f.Fd()), "system.posix_acl_access", acl, 0)
		require.NoError(t, err, "native filesystem must support the ACL test")
		applied = true
		return func() []byte {
			data := make([]byte, 4096)
			n, err := unix.Fgetxattr(int(f.Fd()), "system.posix_acl_access", data)
			require.NoError(t, err)
			return data[:n]
		}
	}
	acl := "user:" + strconv.Itoa(os.Getuid()) + " allow list,readsecurity"
	applied := false
	t.Cleanup(func() {
		if applied {
			output, err := exec.Command("chmod", "-a", acl, target).CombinedOutput()
			require.NoError(t, err, "%s", output)
		}
	})
	output, err := exec.Command("chmod", "+a", acl, target).CombinedOutput()
	require.NoError(t, err, "%s", output)
	applied = true
	return func() []byte {
		output, err := exec.Command("ls", "-lde", target).CombinedOutput()
		require.NoError(t, err, "%s", output)
		return output
	}
}

func TestPublishUnixAddedDirectoryACLRefusesWithoutRepair(t *testing.T) {
	// Catches accepting an ACL added after ownership was established, or
	// repairing it to permit publication rather than preserving the boundary.
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "generations"}[managed], func(t *testing.T) {
			target := publicationTarget(t)
			old := publishOne(t, target)
			aclTarget := target
			if managed {
				aclTarget = filepath.Join(target, "generations")
			}
			readACL := addSyntheticDirectoryACL(t, aclTarget)
			before := readACL()
			selected, err := Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
			require.Error(t, err)
			require.Empty(t, selected.GenerationID)
			pointer, err := os.ReadFile(filepath.Join(target, "CURRENT"))
			require.NoError(t, err)
			require.Equal(t, old.GenerationID+"\n", string(pointer))
			require.Equal(t, before, readACL())
			info, err := os.Stat(aclTarget)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
		})
	}
}

func TestOwnershipUnixBootstrapLockSubstitution(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			base := syntheticPrivateBase(t)
			syntheticSentinel(t, base)
			target := filepath.Join(base, "export")
			_, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{afterBootstrapLock: func() {
				require.NoError(t, os.Rename(filepath.Join(target, lockName), filepath.Join(target, "original-lock")))
				if replace {
					require.NoError(t, os.WriteFile(filepath.Join(target, lockName), []byte("unknown lock"), 0o600))
				}
			}})
			require.Error(t, err)
			if replace {
				require.Equal(t, []byte("unknown lock"), readSyntheticSentinel(t, filepath.Join(target, lockName)))
			}
			_, err = os.Stat(filepath.Join(target, markerName))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestNativeUnixRefusesSubstitutedMoveAndLinkedDestination(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	r, err := acquireOwnedRoot(t.Context(), filepath.Join(base, "export"), ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	stage, id, err := r.staging.createDir("stage", movableEntry)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stage.Close()) })
	require.NoError(t, os.Rename(filepath.Join(r.absolute, ".staging", "stage"), filepath.Join(r.absolute, ".staging", "original")))
	require.NoError(t, os.Mkdir(filepath.Join(r.absolute, ".staging", "stage"), 0o700))
	require.Error(t, moveDirectoryNoReplace(r.staging, "stage", stage, id, r.generations, "selected"))
	f, fileID, err := r.root.createFile("next", movableEntry)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })
	require.NoError(t, os.Symlink(filepath.Join(base, "unrelated"), filepath.Join(r.absolute, "pointer")))
	require.Error(t, replaceFile(r.root, "next", f, fileID, r.root, "pointer"))
}

func TestOwnershipUnixClaimRechecksRootPrivacy(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	target := filepath.Join(base, "export")
	r, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{beforeClaimRecheck: func() {
		require.NoError(t, os.Chmod(target, 0o755))
	}})
	if r != nil {
		require.NoError(t, r.Close())
	}
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(target, markerName))
	require.ErrorIs(t, err, os.ErrNotExist)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}
