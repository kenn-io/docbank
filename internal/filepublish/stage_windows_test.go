//go:build windows

package filepublish

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestPublishAcceptsPathsLongerThanMaxPath(t *testing.T) {
	dir := t.TempDir()
	for len(dir) <= windows.MAX_PATH {
		dir = filepath.Join(dir, strings.Repeat("d", 40))
	}
	require.NoError(t, os.MkdirAll(dir, 0o700))
	destination := filepath.Join(dir, "out.pdf")
	for _, overwrite := range []bool{false, true} {
		staged := filepath.Join(dir, fmt.Sprintf("stage-%t.tmp", overwrite))
		require.NoError(t, os.WriteFile(staged, []byte(strconv.FormatBool(overwrite)), 0o600))
		published, err := Publish(staged, destination, overwrite)
		require.NoError(t, err)
		require.True(t, published)
	}
	moved := filepath.Join(dir, "moved.pdf")
	staged := filepath.Join(dir, "rename.tmp")
	require.NoError(t, os.WriteFile(staged, []byte("rename"), 0o600))
	require.NoError(t, renameNoReplace(staged, moved), "MoveFileW must accept a path longer than MAX_PATH")
	got, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "true", string(got))
}
