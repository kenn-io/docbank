//go:build windows

package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/windows"
)

func TestReportSpoolOverridesPermissiveWindowsTempDACL(t *testing.T) {
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
	require.NoError(t, windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil))

	spool, err := createPrivateReportSpoolAt(parent)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupReportSpool(&spool) })
	require.NoError(t, safefileio.ValidatePrivateDir(spool.dir))
	checked, err := winsecurity.OpenRestrictedCurrentUserFile(spool.path)
	require.NoError(t, err)
	require.NoError(t, checked.Close())
	require.Error(t, os.Rename(spool.dir, filepath.Join(parent, "replaced-spool")),
		"the held directory must not be replaceable while a handle is live")
	_, err = spool.file.Write([]byte("synthetic report"))
	require.NoError(t, err)
	path, dir := spool.path, spool.dir
	cleanupReportSpool(&spool)
	require.NoFileExists(t, path)
	require.NoDirExists(t, dir)
}
