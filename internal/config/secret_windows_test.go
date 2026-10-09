//go:build windows

package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/docbank/internal/winsecurity"
)

func TestSecretWindowsAcceptsReadOnlyOwnerAndRejectsBroadAccess(t *testing.T) {
	for _, broad := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner read only", true: "everyone"}[broad], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			writePrivateSecret(t, path, "synthetic-key")
			user, err := windows.GetCurrentProcessToken().GetTokenUser()
			require.NoError(t, err)
			sid := user.User.Sid
			if broad {
				sid, err = windows.CreateWellKnownSid(windows.WinWorldSid)
				require.NoError(t, err)
			}
			dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
				AccessPermissions: windows.GENERIC_READ,
				AccessMode:        windows.GRANT_ACCESS,
				Inheritance:       windows.NO_INHERITANCE,
				Trustee: windows.TRUSTEE{
					TrusteeForm:  windows.TRUSTEE_IS_SID,
					TrusteeType:  windows.TRUSTEE_TYPE(windows.TRUSTEE_IS_USER),
					TrusteeValue: windows.TrusteeValueFromSID(sid),
				},
			}}, nil)
			require.NoError(t, err)
			require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
				windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
				nil, nil, dacl, nil))
			t.Cleanup(func() { require.NoError(t, winsecurity.RestrictCurrentUserFile(path)) })
			file, err := openSecret(path)
			if broad {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NoError(t, file.Close())
			}
		})
	}
}
