//go:build windows

package blob

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/windows"
)

func TestFilesystemBackendSecuresExistingNamespaceDirectories(t *testing.T) {
	root := t.TempDir()
	shard := filepath.Join(root, "aa")
	require.NoError(t, safefileio.EnsurePrivateDir(shard))
	makeFilesystemNamespaceInsecure(t, root, shard)

	_, err := NewFilesystemBackend(root, nil)
	require.Error(t, err)
	require.NoError(t, EnsureFilesystemNamespace(root))
	backend, err := NewFilesystemBackend(root, nil)
	require.NoError(t, err)
	require.NoError(t, backend.Close())
	require.NoError(t, safefileio.ValidatePrivateDir(root))
	require.NoError(t, safefileio.ValidatePrivateDir(shard))
}

func makeFilesystemNamespaceInsecure(t *testing.T, paths ...string) {
	t.Helper()
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
	for _, path := range paths {
		require.NoError(t, windows.SetNamedSecurityInfo(
			path, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil, nil, dacl, nil,
		))
	}
}
