//go:build !linux

package renderpdf

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/providerutil/sandbox"
)

func TestNewPolicyWithoutRunnerFailsClosedOffLinux(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(executable)
	require.NoError(t, err)
	_, err = NewPolicy(Renderer{
		Executable: executable, ExecutableSHA256: digest(content),
		RuntimeIdentity: testRunnerIdentity,
	}, DefaultLimits())
	require.ErrorIs(t, err, sandbox.ErrUnavailable)
	require.NotEqual(t, "linux", runtime.GOOS)
}
