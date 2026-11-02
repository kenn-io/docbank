package posthog

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The package exists so reporters link without OpenTelemetry.
func TestPackageLinksNoOpenTelemetry(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go command not found")
	}
	out, err := exec.CommandContext(t.Context(), gobin, "list", "-deps", ".").Output()
	require.NoError(t, err)
	for dep := range strings.Lines(string(out)) {
		require.NotContains(t, dep, "go.opentelemetry.io")
	}
}
