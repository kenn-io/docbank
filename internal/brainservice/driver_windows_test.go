package brainservice

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestBrainServiceNativeDriver(t *testing.T) {
	spec := renderSpec("windows")
	source, err := os.Executable()
	require.NoError(t, err)
	exe := filepath.Join(t.TempDir(), "Docbank café $&%", "docbank.exe")
	require.NoError(t, os.MkdirAll(filepath.Dir(exe), 0o700))
	binary, err := os.ReadFile(source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(exe, binary, 0o700))
	spec.Executable = exe
	reg, err := Render("windows", spec)
	require.NoError(t, err)
	var task struct {
		Actions struct {
			Exec struct{ Command, Arguments string }
		}
	}
	require.NoError(t, xml.Unmarshal(reg.Definition, &task))
	// The Windows API is an independent oracle; do not decode then let os/exec
	// re-encode the arguments, which would avoid testing the rendered codec.
	argv, err := windows.DecomposeCommandLine(`"` + task.Actions.Exec.Command + `" ` + task.Actions.Exec.Arguments)
	require.NoError(t, err)
	require.Equal(t, reg.Argv, argv)
	cmd := exec.CommandContext(t.Context(), exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + exe + `" ` + task.Actions.Exec.Arguments}
	cmd.Env = append(os.Environ(), "DOCBANK_TEST_BRAIN_ARGV_CHILD=1")
	out, err := cmd.Output()
	require.NoError(t, err)
	var want strings.Builder
	for _, arg := range reg.Argv {
		fmt.Fprintf(&want, "%q\n", arg)
	}
	require.Equal(t, want.String(), string(out))
}
