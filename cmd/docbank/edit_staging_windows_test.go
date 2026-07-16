//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/windows"
)

func TestMakePrivateEditDirOverridesPermissiveParentDACL(t *testing.T) {
	parent := t.TempDir()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, err)
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(
		parent,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	))

	stage, err := makePrivateEditDirAt(parent)
	require.NoError(t, err)
	removed := false
	t.Cleanup(func() {
		if !removed {
			_ = stage.removeAll()
		}
	})
	require.NoError(t, safefileio.ValidatePrivateDir(stage.path))

	moveTarget := filepath.Join(parent, "replaced-staging")
	require.Error(t, os.Rename(stage.path, moveTarget))

	file, path, err := stage.createFile("notes.txt")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.FileExists(t, path)
	require.NoError(t, stage.removeAll())
	removed = true
	require.NoDirExists(t, stage.path)
}
