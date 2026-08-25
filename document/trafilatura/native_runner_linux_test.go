//go:build linux

package trafilatura

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type nativeRunOutcome struct {
	result IsolatedRunResult
	err    error
}

func TestNativeRunnerUsesExactStdinArgumentsAndCleanEnvironment(t *testing.T) {
	executable := buildIsolatedHelper(t, "echo", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	stdin := []byte("exact supplied bytes\x00remain data, never arguments")
	request := nativeTestRequest(t, runner, executable, stdin, 1<<20)

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
	request := nativeTestRequest(t, runner, executable, []byte("network probe"), 1<<20)

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
	request := nativeTestRequest(t, runner, executable, []byte("unix network probe"), 1<<20)

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
	request := nativeTestRequest(t, runner, executable, []byte("filesystem probe"), 1<<20)

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

func TestNativeFilesystemLandlockDeniesHostFileAccess(t *testing.T) {
	const targetEnvironment = "DOCBANK_TEST_TRAFILATURA_HOST_FILE"
	if target := os.Getenv(targetEnvironment); target != "" {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			os.Exit(70)
		}
		if err := installNativeFilesystemLandlock(); err != nil {
			_, _ = fmt.Fprint(os.Stderr, err)
			os.Exit(77)
		}
		if _, err := os.ReadFile(target); err == nil {
			os.Exit(71)
		}
		if err := os.WriteFile(target, []byte("modified"), 0o600); err == nil {
			os.Exit(72)
		}
		if err := exec.Command("/bin/true").Run(); err != nil {
			os.Exit(73)
		}
		os.Exit(0)
	}

	// The target must be outside the /tmp path explicitly allowed to the subprocess.
	hostDirectory, err := os.MkdirTemp(".", ".trafilatura-landlock-") //nolint:usetesting
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(hostDirectory)) })
	hostPath, err := filepath.Abs(filepath.Join(hostDirectory, "host-only.txt"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(hostPath, []byte("host-only"), 0o600))
	command := exec.Command( //nolint:gosec // os.Args[0] is the trusted current test executable
		os.Args[0], "-test.run=^TestNativeFilesystemLandlockDeniesHostFileAccess$",
	)
	command.Env = append(os.Environ(), targetEnvironment+"="+hostPath)
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 77 {
		t.Skipf("Landlock filesystem isolation unavailable: %s", output)
	}
	require.NoError(t, err, "%s", output)
	content, err := os.ReadFile(hostPath)
	require.NoError(t, err)
	assert.Equal(t, "host-only", string(content))
}

func TestNativeSeccompDeniesPathnameUnixSockets(t *testing.T) {
	const helperEnvironment = "DOCBANK_TEST_TRAFILATURA_SECCOMP"
	if socketPath := os.Getenv(helperEnvironment); socketPath != "" {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			os.Exit(10)
		}
		if err := installNativeNetworkSeccomp(); err != nil {
			os.Exit(11)
		}
		connection, err := net.DialTimeout("unix", socketPath, time.Second)
		if connection != nil {
			_ = connection.Close()
		}
		if !errors.Is(err, unix.EPERM) {
			os.Exit(12)
		}
		os.Exit(0)
	}

	socketPath := filepath.Join(t.TempDir(), "seccomp.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	require.NoError(t, err, "the host listener must be reachable before filtering")
	require.NoError(t, connection.Close())
	command := exec.Command( //nolint:gosec // os.Args[0] is the trusted current test executable
		os.Args[0], "-test.run=^TestNativeSeccompDeniesPathnameUnixSockets$",
	)
	command.Env = append(os.Environ(), helperEnvironment+"="+socketPath)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestNativeSeccompFilterDeniesX32ABI(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x32 ABI exists only on amd64")
	}
	filters, err := buildNativeNetworkSeccompFilters(unix.AUDIT_ARCH_X86_64)
	require.NoError(t, err)
	denied := unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	for name, syscallNumber := range map[string]uint32{
		"socket":   unix.SYS_SOCKET,
		"connect":  unix.SYS_CONNECT,
		"io_uring": unix.SYS_IO_URING_SETUP,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, denied, evaluateNativeSeccomp(t, filters, syscallNumber))
			assert.Equal(t, denied, evaluateNativeSeccomp(t, filters, syscallNumber|nativeX32SyscallBit))
		})
	}
	assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW),
		evaluateNativeSeccomp(t, filters, unix.SYS_GETPID))
	assert.Equal(t, denied,
		evaluateNativeSeccomp(t, filters, unix.SYS_GETPID|nativeX32SyscallBit))
}

func TestNativeLauncherRequiresSealedMatchingInheritedControl(t *testing.T) {
	control, token, err := openNativeLaunchControl()
	require.NoError(t, err)
	defer func() { _ = control.Close() }()
	arguments := []string{"/proc/self/exe", nativeLauncherMarker, token, "/opt/trafilatura-bridge"}

	executable, authenticated := authenticatedNativeLaunch(arguments, int(control.Fd()))
	assert.True(t, authenticated)
	assert.Equal(t, "/opt/trafilatura-bridge", executable)

	_, authenticated = authenticatedNativeLaunch(arguments, -1)
	assert.False(t, authenticated)
	arguments[2] = strings.Repeat("0", nativeLauncherTokenBytes*2)
	_, authenticated = authenticatedNativeLaunch(arguments, int(control.Fd()))
	assert.False(t, authenticated)
}

func TestNativeRunnerCancellationReapsDescendantProcessTree(t *testing.T) {
	executable := buildIsolatedHelper(t, "descendant", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("descendant probe"), 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan nativeRunOutcome, 1)
	started := time.Now()
	go func() {
		result, runErr := runner.Run(ctx, request)
		finished <- nativeRunOutcome{result: result, err: runErr}
	}()

	select {
	case outcome := <-finished:
		skipUnavailableNativeIsolation(t, outcome.err)
		require.FailNow(t, "isolated runner exited before cancellation", "%v", outcome.err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	outcome := <-finished
	require.ErrorIs(t, outcome.err, context.Canceled)
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.True(t, outcome.result.Attestation.ProcessTreeContained)
	assert.Contains(t, string(outcome.result.Stdout), "spawned")
	assert.Contains(t, string(outcome.result.Stdout), "descendant-ready")
}

func TestNativeRunnerTerminatesPromptlyOnStdoutOverflow(t *testing.T) {
	executable := buildIsolatedHelper(t, "overflow", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("overflow probe"), 1024)
	started := time.Now()

	result, err := runner.Run(t.Context(), request)
	skipUnavailableNativeIsolation(t, err)
	require.ErrorIs(t, err, ErrChildOutputTooLarge)
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.LessOrEqual(t, int64(len(result.Stdout)), request.MaxStdoutBytes)
}

func TestNativeRunnerDoesNotMisclassifyBridgeExit125AsLauncherFailure(t *testing.T) {
	executable := buildIsolatedHelper(t, "exit-125", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("exit probe"), 1<<20)

	_, err = runner.Run(t.Context(), request)
	skipUnavailableNativeIsolation(t, err)
	require.ErrorIs(t, err, ErrChildFailed)
	require.NotErrorIs(t, err, ErrIsolationUnavailable)
}

func TestNativeRunErrorClassificationUsesOutOfBandLauncherStatus(t *testing.T) {
	runErr := exec.Command("/bin/sh", "-c", "exit 125").Run()
	require.Error(t, runErr)
	require.ErrorIs(t, classifyNativeRunError(runErr, false), ErrChildFailed)
	require.ErrorIs(t, classifyNativeRunError(runErr, true), ErrIsolationUnavailable)
}

func TestNativeRunnerHonorsCancellationBeforeExecutablePreparation(t *testing.T) {
	executable := buildIsolatedHelper(t, "echo", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("canceled probe"), 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = runner.Run(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
}

func TestNativeRunnerNeverLaunchesExecutableContentOutsidePinnedDigest(t *testing.T) {
	executable := buildIsolatedHelper(t, "echo", "")
	replacement := buildIsolatedHelper(t, "replacement", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("identity probe"), 1<<20)
	require.NoError(t, os.Rename(replacement, executable))

	_, err = runner.Run(t.Context(), request)
	require.ErrorIs(t, err, ErrIsolationUnavailable)
}

func TestVerifiedExecutableContentCannotChangeAfterDigestVerification(t *testing.T) {
	executable := buildIsolatedHelper(t, "echo", "")
	replacement := buildIsolatedHelper(t, "replacement", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("identity probe"), 1<<20)
	want, err := os.ReadFile(executable)
	require.NoError(t, err)
	prepared, err := openVerifiedExecutable(t.Context(), request)
	require.NoError(t, err)
	defer func() { _ = prepared.Close() }()
	replacementBytes, err := os.ReadFile(replacement)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(executable, replacementBytes, 0o700))

	got, err := os.ReadFile("/proc/self/fd/" + strconv.FormatUint(uint64(prepared.Fd()), 10))
	require.NoError(t, err)
	wantDigest := sha256.Sum256(want)
	gotDigest := sha256.Sum256(got)
	assert.Equal(t, wantDigest, gotDigest)
}

func TestNewUsesNativeRunnerWhenNoneIsInjected(t *testing.T) {
	profile := testProfile(t, helperExecutable(t, "complete"), time.Second, 1<<20)
	profile.Runner = nil

	provider, err := New(profile)
	require.NoError(t, err)
	assert.Equal(t, nativeRunnerIdentity, provider.runnerIdentity)
}

func nativeTestRequest(
	t *testing.T, runner IsolatedRunner, executable string, stdin []byte, maxStdout int64,
) IsolatedRunRequest {
	t.Helper()
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	provider := &Provider{
		executable: executable, executableSHA256: hex.EncodeToString(digest[:]),
		runnerIdentity: runner.Identity(), environment: cleanEnvironment(),
	}
	return provider.isolatedRequest(stdin, maxStdout)
}

func buildIsolatedHelper(t *testing.T, mode, networkAddress string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "isolated-helper")
	ldflags := strings.Join([]string{
		"-X=main.mode=" + mode,
		"-X=main.networkAddress=" + networkAddress,
	}, " ")
	command := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags,
		"-o", target, "./testdata/isolatedhelper")
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

func evaluateNativeSeccomp(
	t *testing.T, filters []unix.SockFilter, syscallNumber uint32,
) uint32 {
	t.Helper()
	accumulator := uint32(0)
	for programCounter, steps := 0, 0; programCounter < len(filters) && steps <= len(filters); steps++ {
		instruction := filters[programCounter]
		switch instruction.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			switch instruction.K {
			case 0:
				accumulator = syscallNumber
			case 4:
				accumulator = unix.AUDIT_ARCH_X86_64
			default:
				require.FailNow(t, "unexpected seccomp load offset", "%d", instruction.K)
			}
			programCounter++
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if accumulator == instruction.K {
				programCounter += int(instruction.Jt) + 1
			} else {
				programCounter += int(instruction.Jf) + 1
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if accumulator&instruction.K != 0 {
				programCounter += int(instruction.Jt) + 1
			} else {
				programCounter += int(instruction.Jf) + 1
			}
		case unix.BPF_RET | unix.BPF_K:
			return instruction.K
		default:
			require.FailNow(t, "unexpected seccomp instruction", "%#x", instruction.Code)
		}
	}
	require.FailNow(t, "seccomp filter did not return")
	return 0
}
