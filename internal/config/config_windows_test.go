//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestLoadWindowsRejectsReparsePoint(t *testing.T) {
	dir := privateTestConfigDir(t)
	target := filepath.Join(t.TempDir(), "attacker.toml")
	require.NoError(t, os.WriteFile(target, []byte("[server]\napi_key = \"known\"\n"), 0o600))
	if err := os.Symlink(target, filepath.Join(dir, "config.toml")); err != nil {
		t.Skipf("creating a Windows symlink requires developer mode: %v", err)
	}

	_, err := Load(dir)
	require.ErrorContains(t, err, "reparse point")
}

func TestLoadWindowsRejectsBroadConfigDACL(t *testing.T) {
	dir := privateTestConfigDir(t)
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[server]\napi_key = \"known\"\n"), 0o600))
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, err)
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_TYPE(windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	))

	_, err = Load(dir)
	require.ErrorContains(t, err, "unexpected principal")
}
