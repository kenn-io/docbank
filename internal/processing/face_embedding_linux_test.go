package processing

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func withFaceDescriptorBudget(t *testing.T, run func()) {
	t.Helper()
	if os.Getenv("DOCBANK_FACE_DESCRIPTOR_CHILD") != "1" {
		executable, err := os.Executable()
		require.NoError(t, err)
		command := exec.CommandContext(t.Context(), executable, "-test.run=^TestFaceEmbeddingCrowdFitsDescriptorBudget$", "-test.timeout=90s")
		command.Env = append(os.Environ(), "DOCBANK_FACE_DESCRIPTOR_CHILD=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	var limit syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit))
	limit.Cur = min(limit.Max, 256)
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit))
	run()
}
