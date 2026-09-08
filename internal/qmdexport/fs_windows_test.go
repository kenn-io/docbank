package qmdexport

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestNativeWindowsStablePinsAndMovableReaders(t *testing.T) {
	for _, root := range []string{`C:\`, "C:/", `\\server\share`, `\\server\share\`} {
		require.True(t, filesystemRoot(root))
	}
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	target := filepath.Join(base, "export")
	r, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	require.NoError(t, preflightRoot(t.Context(), strings.ReplaceAll(target, `\`, "/")))
	require.Error(t, os.Rename(target, filepath.Join(base, "moved")))
	require.Error(t, os.Remove(filepath.Join(target, lockName)))
	stage, id, err := r.staging.createDir("stage", movableEntry)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stage.Close()) })
	stable, _, err := r.staging.openDir("stage")
	if stable != nil {
		_ = stable.Close()
	}
	require.Error(t, err, "a stable open must not silently share a movable creation handle")
	reader, _, err := r.staging.openMovableDir("stage")
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.NoError(t, moveDirectoryNoReplace(r.staging, "stage", stage, id, r.generations, "placed"))
	require.NoError(t, stage.Close())
	require.NoError(t, r.Close())
	require.NoError(t, os.Rename(target, filepath.Join(base, "moved")), "rename must work after stable pins close")
}

func TestOwnershipWindowsRejectsReparseChain(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	realTarget := filepath.Join(base, "real")
	r, err := acquireOwnedRoot(t.Context(), realTarget, ownershipHooks{})
	require.NoError(t, err)
	require.NoError(t, r.Close())
	alias := filepath.Join(base, "alias")
	output, err := exec.Command("cmd", "/c", "mklink", "/J", alias, realTarget).CombinedOutput()
	require.NoError(t, err, "%s", output)
	for _, target := range []string{alias, filepath.Join(alias, "child")} {
		require.Error(t, preflightRoot(t.Context(), target))
		_, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
		require.Error(t, err)
	}
}

func TestOwnershipWindowsRejectsBroadAndUnprotectedDACL(t *testing.T) {
	for _, broad := range []bool{true, false} {
		t.Run(map[bool]string{true: "broad", false: "unprotected"}[broad], func(t *testing.T) {
			base := syntheticPrivateBase(t)
			syntheticSentinel(t, base)
			target := filepath.Join(base, "export")
			r, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
			require.NoError(t, err)
			require.NoError(t, r.Close())
			path, err := windows.UTF16PtrFromString(target)
			require.NoError(t, err)
			h, err := windows.CreateFile(path, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			require.NoError(t, err)
			sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			require.NoError(t, err)
			dacl, _, err := sd.DACL()
			require.NoError(t, err)
			flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
			if broad {
				sd, err = windows.SecurityDescriptorFromString("D:P(A;OICI;GA;;;WD)")
				require.NoError(t, err)
				dacl, _, err = sd.DACL()
				require.NoError(t, err)
				flags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
			}
			require.NoError(t, windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil))
			before, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			require.NoError(t, err)
			require.Error(t, preflightRoot(t.Context(), target))
			_, err = acquireOwnedRoot(t.Context(), target, ownershipHooks{})
			require.Error(t, err)
			after, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			require.NoError(t, err)
			require.Equal(t, before.String(), after.String())
			require.NoError(t, windows.CloseHandle(h))
		})
	}
}
