package main

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkCLIStartup includes process launch and package initialization, which
// benchmarks calling Execute in the test process cannot measure. Building the
// binary is excluded; these commands do not start or contact a daemon. The
// shared lifecycle test helper builds a CGO-enabled binary.
func BenchmarkCLIStartup(b *testing.B) {
	bin := buildDocbank(b)
	b.Setenv("DOCBANK_HOME", b.TempDir())
	for _, arg := range []string{"version", "--help"} {
		b.Run(arg, func(b *testing.B) {
			for b.Loop() {
				out, err := exec.CommandContext(b.Context(), bin, arg).CombinedOutput()
				require.NoError(b, err, string(out))
			}
		})
	}
}
