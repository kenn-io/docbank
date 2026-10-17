package redactiontest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/pagerender"
)

// PageRuntime runs the real compiled inspector under the configured process limits.
func PageRuntime(t *testing.T) *pagerender.Runtime {
	t.Helper()
	runtime, err := pagerender.New(t.Context(), PageProfile(t))
	require.NoError(t, err)
	return runtime
}

// PageProfile pins the same real tools for in-process and daemon qualifications.
func PageProfile(t *testing.T) pagerender.Profile {
	t.Helper()
	if !pagerender.SupportedPlatform() {
		t.Skip("page runtime is Linux amd64 only")
	}
	renderer := os.Getenv("DOCBANK_TEST_PDFTOPPM")
	var err error
	if renderer == "" {
		renderer, err = exec.LookPath("pdftoppm")
	}
	if err != nil {
		t.Skip("Poppler unavailable")
	}
	limiter, err := exec.LookPath("prlimit")
	if err != nil {
		t.Skip("prlimit unavailable")
	}
	inspector := filepath.Join(t.TempDir(), "docbank-page-inspect")
	//nolint:gosec // Build our inspector into the private test directory.
	command := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", inspector,
		"go.kenn.io/docbank/document/pagerender/cmd/docbank-page-inspect")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	pin := func(path string) pagerender.Executable {
		resolved, err := filepath.EvalSymlinks(path)
		require.NoError(t, err)
		data, err := os.ReadFile(resolved) //nolint:gosec // Operator-selected test tool, pinned before execution.
		require.NoError(t, err)
		hash := sha256.Sum256(data)
		return pagerender.Executable{Path: resolved, SHA256: hex.EncodeToString(hash[:])}
	}
	return pagerender.Profile{Inspector: pin(inspector),
		Renderer: pin(renderer), Limiter: pin(limiter),
		DeploymentIdentity: "synthetic-font-qualification"}
}
