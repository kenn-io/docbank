//go:build linux

package trafilatura

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsScriptWithNativeRunner(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "trafilatura-bridge")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	profile := testProfile(t, executable, time.Second, 1<<20)
	profile.Runner = nil

	_, err := New(profile)
	require.ErrorContains(t, err, "native runner requires an ELF executable")
}

func TestNativeRunnerUsesExactStdinArgumentsAndCleanEnvironment(t *testing.T) {
	executable := buildIsolatedHelper(t, "echo", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	stdin := []byte("exact supplied bytes\x00remain data, never arguments")
	request := nativeTestRequest(t, runner, executable, stdin)

	result, err := runner.Run(t.Context(), request)
	skipUnavailableNativeIsolation(t, err)
	require.NoError(t, err)

	var response struct {
		Arguments   []string `json:"arguments"`
		Environment []string `json:"environment"`
		StdinSHA256 string   `json:"stdin_sha256"`
	}
	require.NoError(t, json.Unmarshal(result.Stdout, &response))
	digest := sha256.Sum256(stdin)
	assert.Equal(t, hex.EncodeToString(digest[:]), response.StdinSHA256)
	assert.Equal(t, []string{"--protocol", protocolVersion}, response.Arguments)
	assert.Equal(t, cleanEnvironment(), response.Environment)
	assert.Equal(t, nativeRunnerIdentity, result.Attestation.RunnerIdentity)
	assert.True(t, result.Attestation.NetworkDisabled)
	assert.True(t, result.Attestation.ProcessTreeContained)
	assert.True(t, result.Attestation.DigestVerifiedLaunch)
	assert.True(t, result.Attestation.FilesystemIsolated)
}

func TestNativeRunnerDeniesLoopbackNetworkAccess(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	executable := buildIsolatedHelper(t, "network", listener.Addr().String())
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("network probe"))

	result, err := runner.Run(t.Context(), request)
	skipUnavailableNativeIsolation(t, err)
	require.NoError(t, err)
	assert.Equal(t, "denied", string(result.Stdout))
}

func TestNativeRunnerDeniesHostPathnameUnixSocketAccess(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	executable := buildIsolatedHelper(t, "unix-network", socketPath)
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("unix network probe"))

	result, err := runner.Run(t.Context(), request)
	skipUnavailableNativeIsolation(t, err)
	require.NoError(t, err)
	assert.Equal(t, "denied", string(result.Stdout))
}

func TestNativeRunnerCannotReadOrModifyHostFiles(t *testing.T) {
	// The target must remain on a host mount because the sandbox replaces /tmp.
	hostDirectory, err := os.MkdirTemp(".", ".trafilatura-host-") //nolint:usetesting
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(hostDirectory)) })
	hostPath, err := filepath.Abs(filepath.Join(hostDirectory, "host-only.txt"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(hostPath, []byte("host-only"), 0o600))
	before, err := os.Stat(hostPath)
	require.NoError(t, err)
	executable := buildIsolatedHelper(t, "host-file", hostPath)
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("filesystem probe"))

	result, err := runner.Run(t.Context(), request)
	skipUnavailableNativeIsolation(t, err)
	require.NoError(t, err)
	assert.Equal(t, "denied", string(result.Stdout))
	content, err := os.ReadFile(hostPath)
	require.NoError(t, err)
	assert.Equal(t, "host-only", string(content))
	after, err := os.Stat(hostPath)
	require.NoError(t, err)
	assert.Equal(t, before.Mode(), after.Mode())
	assert.Equal(t, before.ModTime(), after.ModTime())
}

func TestNewUsesNativeRunnerWhenNoneIsInjected(t *testing.T) {
	profile := testProfile(t, buildIsolatedHelper(t, "echo", ""), time.Second, 1<<20)
	profile.Runner = nil

	provider, err := New(profile)
	require.NoError(t, err)
	assert.Equal(t, nativeRunnerIdentity, provider.runnerIdentity)
}

func nativeTestRequest(
	t *testing.T, runner IsolatedRunner, executable string, stdin []byte,
) IsolatedRunRequest {
	t.Helper()
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	provider := &Provider{
		executable: executable, executableSHA256: hex.EncodeToString(digest[:]),
		runnerIdentity: runner.Identity(), environment: cleanEnvironment(),
	}
	return provider.isolatedRequest(stdin, 1<<20)
}

func buildIsolatedHelper(t *testing.T, mode, networkAddress string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "isolated-helper")
	ldflags := strings.Join([]string{
		"-X=main.mode=" + mode,
		"-X=main.networkAddress=" + networkAddress,
	}, " ")
	command := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags,
		"-o", target, "../internal/providerutil/sandbox/testdata/sandboxhelper")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return target
}

func skipUnavailableNativeIsolation(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, ErrIsolationUnavailable) {
		t.Skipf("native Linux namespace isolation unavailable: %v", err)
	}
}
