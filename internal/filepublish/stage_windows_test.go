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
	for _, rooted := range []bool{false, true} {
		t.Run(fmt.Sprintf("rooted=%t", rooted), func(t *testing.T) {
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

			if rooted {
				root, err := os.OpenRoot(parent)
				require.NoError(t, err)
				defer func() { _ = root.Close() }()
				rootStage, err := CreateRootStage(root)
				require.NoError(t, err)
				defer func() { require.NoError(t, rootStage.Cleanup()) }()
				restricted, err := winsecurity.OpenRestrictedCurrentUserFile(filepath.Join(parent, rootStage.name))
				require.NoError(t, err)
				require.NoError(t, restricted.Close())
				require.NoError(t, rootStage.MakeParentDirectories(filepath.Join("private", "asset", "photo.jpg")))
				for _, dir := range []string{"private", filepath.Join("private", "asset")} {
					require.NoError(t, safefileio.ValidatePrivateDir(filepath.Join(parent, dir)))
				}
			} else {
				stage, err := CreateStage(parent, ".docbank-test-")
				require.NoError(t, err)
				t.Cleanup(func() { _ = stage.Cleanup() })
				require.NoError(t, safefileio.ValidatePrivateDir(stage.dir))
				restricted, err := winsecurity.OpenRestrictedCurrentUserFile(stage.Path())
				require.NoError(t, err)
				require.NoError(t, restricted.Close())
			}
		})
	}
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
	got, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "true", string(got))
}
