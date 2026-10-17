//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestClientStartupRepairsWindowsConfigDACL(t *testing.T) {
	bin := buildDocbank(t)
	for _, args := range [][]string{{"search", "synthetic-query"}, {"daemon", "start"}} {
		t.Run(args[0], func(t *testing.T) {
			root := scopedHome(t)
			path := filepath.Join(root, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[server]\nidle_timeout='0'\n"), 0600))
			everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
			require.NoError(t, err)
			dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
				AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS,
				Inheritance: windows.NO_INHERITANCE,
				Trustee:     windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_TYPE(windows.TRUSTEE_IS_WELL_KNOWN_GROUP), TrusteeValue: windows.TrusteeValueFromSID(everyone)},
			}}, nil)
			require.NoError(t, err)
			require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil))
			_, err = config.Load(root)
			require.ErrorContains(t, err, "unexpected principal")
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, err := daemonconn.Stop(ctx, root)
				require.NoError(t, err)
			})
			child := exec.CommandContext(t.Context(), bin, args...)
			output, err := child.CombinedOutput()
			require.NoError(t, err, string(output))
			_, err = config.Load(root)
			require.NoError(t, err, "startup must repair config permissions before preflight")
		})
	}
}
