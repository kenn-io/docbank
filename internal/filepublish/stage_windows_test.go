//go:build windows

package filepublish

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/winsecurity"
	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/windows"
)

func TestCreateStageOverridesPermissiveParentDACL(t *testing.T) {
	parent := t.TempDir()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, err)
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone)},
	}}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil))

	stage, err := CreateStage(parent, ".docbank-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = stage.Cleanup() })
	require.NoError(t, safefileio.ValidatePrivateDir(stage.dir))
	restricted, err := winsecurity.OpenRestrictedCurrentUserFile(stage.Path())
	require.NoError(t, err)
	require.NoError(t, restricted.Close())
}
