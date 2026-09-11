package isolate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativePolicyAdmitsOnlyFixedProtocols(t *testing.T) {
	digest := sha256.Sum256([]byte("synthetic"))
	for _, protocol := range []string{"docbank-trafilatura/v2", "docbank-pymupdf/v1"} {
		t.Run(protocol, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), "synthetic-bridge")
			request := IsolatedRunRequest{Executable: executable, ExecutableSHA256: hex.EncodeToString(digest[:]), Arguments: []string{"--protocol", protocol}, Environment: nativeEnvironment(), Directory: filepath.Dir(executable), Stdin: []byte("synthetic"), StdinSHA256: hex.EncodeToString(digest[:]), MaxStdoutBytes: 1024, Requirements: IsolationRequirements{NetworkDisabled: true, KillProcessTree: true, VerifyExecutableSHA256: true}}
			request.PolicyFingerprint = RequestPolicyFingerprint(nativeRunnerIdentity, request)
			require.NoError(t, validateNativeRequest(request))
			for _, test := range []struct {
				name   string
				mutate func(*IsolatedRunRequest)
			}{
				{"unknown protocol", func(r *IsolatedRunRequest) { r.Arguments = []string{"--protocol", "unknown"} }},
				{"extra argument", func(r *IsolatedRunRequest) { r.Arguments = append(r.Arguments, "--extra") }},
				{"changed environment", func(r *IsolatedRunRequest) { r.Environment = []string{"TZ=elsewhere"} }},
				{"extra environment", func(r *IsolatedRunRequest) { r.Environment = append(r.Environment, "PRIVATE=value") }},
				{"relative executable", func(r *IsolatedRunRequest) { r.Executable = "bridge" }},
				{"wrong directory", func(r *IsolatedRunRequest) { r.Directory = "elsewhere" }},
				{"stdin substitution", func(r *IsolatedRunRequest) { r.Stdin = []byte("changed") }},
				{"invalid digest", func(r *IsolatedRunRequest) { r.ExecutableSHA256 = strings.Repeat("z", 64) }},
				{"zero limit", func(r *IsolatedRunRequest) { r.MaxStdoutBytes = 0 }},
				{"excessive limit", func(r *IsolatedRunRequest) { r.MaxStdoutBytes = MaxResponseBytes + 1 }},
				{"network required", func(r *IsolatedRunRequest) { r.Requirements.NetworkDisabled = false }},
				{"tree required", func(r *IsolatedRunRequest) { r.Requirements.KillProcessTree = false }},
				{"digest required", func(r *IsolatedRunRequest) { r.Requirements.VerifyExecutableSHA256 = false }},
			} {
				t.Run(test.name, func(t *testing.T) {
					changed := request
					test.mutate(&changed)
					changed.PolicyFingerprint = RequestPolicyFingerprint(nativeRunnerIdentity, changed)
					require.Error(t, validateNativeRequest(changed))
				})
			}
			request.PolicyFingerprint = strings.Repeat("0", 64)
			require.Error(t, validateNativeRequest(request))
		})
	}
}

func TestNativeIdentityAndUnsupportedHostRefusal(t *testing.T) {
	digest := sha256.Sum256([]byte(nativeRunnerDescriptor))
	assert.Equal(t, nativeRunnerIdentity, "sha256:"+hex.EncodeToString(digest[:]))
	assert.Equal(t, "sha256:024e545bfc708239377cad1d6ec82674b536252c6206af46fbeb63fe376ead1b", nativeRunnerIdentity)
	runner, err := NewNativeRunner()
	if runtime.GOOS != "linux" {
		require.ErrorIs(t, err, ErrIsolationUnavailable)
		assert.Nil(t, runner)
	} else {
		require.NoError(t, err)
		assert.Equal(t, nativeRunnerIdentity, runner.Identity())
	}
}

func TestHashExecutableBoundsAndIdentity(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "bridge")
	require.NoError(t, os.WriteFile(executable, []byte("synthetic"), 0o700))
	digest, err := HashExecutable(t.Context(), executable)
	require.NoError(t, err)
	expected := sha256.Sum256([]byte("synthetic"))
	assert.Equal(t, hex.EncodeToString(expected[:]), digest)
	require.NoError(t, os.WriteFile(executable, nil, 0o700))
	_, err = HashExecutable(t.Context(), executable)
	require.Error(t, err)
	_, err = HashExecutable(t.Context(), filepath.Dir(executable))
	require.Error(t, err)
}
