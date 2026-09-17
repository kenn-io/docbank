//go:build linux

package sandbox

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

type sandboxRunOutcome struct {
	result Result
	err    error
}

func TestNativeRunnerUsesExactStdinArgumentsAndCleanEnvironment(t *testing.T) {
	executable := buildSandboxHelper(t, "echo", "", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	stdin := []byte("exact supplied bytes\x00remain data, never arguments")
	request := sandboxTestRequest(t, executable, stdin, 1<<20)

	result, err := runner.Run(t.Context(), request)
	skipUnavailableSandbox(t, err)
	require.NoError(t, err)
	var response struct {
		Arguments   []string `json:"arguments"`
		Environment []string `json:"environment"`
		StdinSHA256 string   `json:"stdin_sha256"`
	}
	require.NoError(t, json.Unmarshal(result.Stdout, &response))
	digest := sha256.Sum256(stdin)
	assert.Equal(t, hex.EncodeToString(digest[:]), response.StdinSHA256)
	assert.Equal(t, []string{"--protocol", "sandbox"}, response.Arguments)
	assert.Equal(t, cleanSandboxEnvironment(), response.Environment)
	assert.Equal(t, NativeRunnerIdentity, result.Attestation.RunnerIdentity)
	assert.True(t, result.Attestation.NetworkDisabled)
	assert.True(t, result.Attestation.ProcessTreeContained)
	assert.True(t, result.Attestation.DigestVerifiedLaunch)
	assert.True(t, result.Attestation.FilesystemIsolated)
}

func TestNativeRunnerDeniesLoopbackNetworkAccess(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	executable := buildSandboxHelper(t, "network", listener.Addr().String(), "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("network probe"), 1<<20)
	result, err := runner.Run(t.Context(), request)
	skipUnavailableSandbox(t, err)
	require.NoError(t, err)
	assert.Equal(t, "denied", string(result.Stdout))
}

func TestNativeRunnerDeniesHostPathnameUnixSocketAccess(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	executable := buildSandboxHelper(t, "unix-network", socketPath, "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("unix network probe"), 1<<20)
	result, err := runner.Run(t.Context(), request)
	skipUnavailableSandbox(t, err)
	require.NoError(t, err)
	assert.Equal(t, "denied", string(result.Stdout))
}

func TestNativeRunnerCannotReadOrModifyHostFiles(t *testing.T) {
	hostDirectory, err := os.MkdirTemp(".", ".sandbox-host-") //nolint:usetesting
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(hostDirectory)) })
	hostPath, err := filepath.Abs(filepath.Join(hostDirectory, "host-only.txt"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(hostPath, []byte("host-only"), 0o600))
	before, err := os.Stat(hostPath)
	require.NoError(t, err)
	executable := buildSandboxHelper(t, "host-file", hostPath, "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("filesystem probe"), 1<<20)
	result, err := runner.Run(t.Context(), request)
	skipUnavailableSandbox(t, err)
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

func TestFilesystemLandlockDeniesHostFileAccess(t *testing.T) {
	const targetEnvironment = "DOCBANK_TEST_SANDBOX_HOST_FILE"
	if target := os.Getenv(targetEnvironment); target != "" {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			os.Exit(70)
		}
		if err := installFilesystemLandlock(nil); err != nil {
			_, _ = fmt.Fprint(os.Stderr, err)
			os.Exit(77)
		}
		if _, err := os.ReadFile(target); err == nil {
			os.Exit(71)
		}
		if err := os.WriteFile(target, []byte("modified"), 0o600); err == nil {
			os.Exit(72)
		}
		os.Exit(0)
	}
	hostDirectory, err := os.MkdirTemp(".", ".sandbox-landlock-") //nolint:usetesting
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(hostDirectory)) })
	hostPath, err := filepath.Abs(filepath.Join(hostDirectory, "host-only.txt"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(hostPath, []byte("host-only"), 0o600))
	command := exec.Command( //nolint:gosec // os.Args[0] is the trusted current test executable
		os.Args[0], "-test.run=^TestFilesystemLandlockDeniesHostFileAccess$",
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

func TestSeccompDeniesPathnameUnixSockets(t *testing.T) {
	const helperEnvironment = "DOCBANK_TEST_SANDBOX_SECCOMP"
	if socketPath := os.Getenv(helperEnvironment); socketPath != "" {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			os.Exit(10)
		}
		if err := installNetworkSeccomp(false); err != nil {
			os.Exit(11)
		}
		connection, err := net.DialTimeout("unix", socketPath, time.Second)
		if connection != nil {
			_ = connection.Close()
		}
		if err != nil && !errors.Is(err, unix.EPERM) {
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
		os.Args[0], "-test.run=^TestSeccompDeniesPathnameUnixSockets$",
	)
	command.Env = append(os.Environ(), helperEnvironment+"="+socketPath)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestSeccompFilterDeniesX32ABI(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x32 ABI exists only on amd64")
	}
	filters, err := buildNetworkSeccompFilters(unix.AUDIT_ARCH_X86_64, false)
	require.NoError(t, err)
	denied := unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	for name, syscallNumber := range map[string]uint32{
		"socket":   unix.SYS_SOCKET,
		"connect":  unix.SYS_CONNECT,
		"io_uring": unix.SYS_IO_URING_SETUP,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, denied, evaluateSeccomp(t, filters, syscallNumber))
			assert.Equal(t, denied, evaluateSeccomp(t, filters, syscallNumber|nativeX32SyscallBit))
		})
	}
	assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW), evaluateSeccomp(t, filters, unix.SYS_GETPID))
	assert.Equal(t, denied, evaluateSeccomp(t, filters, unix.SYS_GETPID|nativeX32SyscallBit))
}

func TestSeccompPolicyScopedLocalIPC(t *testing.T) {
	strict, err := buildNetworkSeccompFilters(unix.AUDIT_ARCH_X86_64, false)
	require.NoError(t, err)
	local, err := buildNetworkSeccompFilters(unix.AUDIT_ARCH_X86_64, true)
	require.NoError(t, err)
	denied := unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	for _, syscallNumber := range []uint32{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT, unix.SYS_BIND,
		unix.SYS_LISTEN, unix.SYS_ACCEPT, unix.SYS_ACCEPT4,
	} {
		assert.Equal(t, denied, evaluateSeccompDomain(t, strict, syscallNumber, unix.AF_UNIX))
	}
	for _, domain := range []uint32{unix.AF_INET, unix.AF_INET6, unix.AF_NETLINK} {
		assert.Equal(t, denied, evaluateSeccompDomain(t, local, unix.SYS_SOCKET, domain))
		assert.Equal(t, denied, evaluateSeccompDomain(t, local, unix.SYS_SOCKETPAIR, domain))
	}
	assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW), evaluateSeccompDomain(t, local, unix.SYS_SOCKET, unix.AF_UNIX))
	assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW), evaluateSeccompDomain(t, local, unix.SYS_SOCKETPAIR, unix.AF_UNIX))
	for _, syscallNumber := range []uint32{unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN, unix.SYS_ACCEPT, unix.SYS_ACCEPT4} {
		assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW), evaluateSeccompDomain(t, local, syscallNumber, unix.AF_UNIX))
	}
	assert.Equal(t, denied, evaluateSeccompDomain(t, local, unix.SYS_SENDMSG, unix.AF_UNIX))
}

func TestLauncherRequiresSealedMatchingInheritedControl(t *testing.T) {
	executable := buildSandboxHelper(t, "echo", "", "")
	stdin := []byte("auth")
	_, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, stdin, 1<<20)
	request.Policy.AllowLocalIPC = true
	request.Policy.Supervision = Supervision{
		Mode: SupervisedFileMode, InputName: "input", OutputName: "output",
		WorkBytes: 1 << 20, MaxOutputBytes: 1 << 20,
	}
	control, token, err := openLaunchControl(request)
	require.NoError(t, err)
	defer func() { _ = control.Close() }()
	arguments := []string{"/proc/self/exe", launcherMarker, token, request.Policy.Executable}
	decoded, authenticated := authenticatedLaunch(arguments, int(control.Fd()))
	assert.True(t, authenticated)
	assert.Equal(t, request.Policy.Executable, decoded.Policy.Executable)
	assert.True(t, decoded.Policy.AllowLocalIPC)
	_, authenticated = authenticatedLaunch(arguments, -1)
	assert.False(t, authenticated)
	arguments[2] = strings.Repeat("0", launcherTokenBytes*2)
	_, authenticated = authenticatedLaunch(arguments, int(control.Fd()))
	assert.False(t, authenticated)
}

func TestSupervisedRunnerReturnsDeclaredOutput(t *testing.T) {
	executable := buildSandboxHelper(t, "file-output", "", "result.bin")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	stdin := []byte("supervised input")
	request := sandboxTestRequest(t, executable, stdin, 1<<20)
	request.Policy.Supervision = Supervision{
		Mode: SupervisedFileMode, InputName: "source.docx", OutputName: "result.bin",
		WorkBytes: 1 << 20, MaxOutputBytes: 1 << 20,
	}
	request.Policy.AllowLocalIPC = true
	require.NoError(t, request.validate())
	result, err := runner.Run(t.Context(), request)
	skipUnavailableSandbox(t, err)
	require.NoError(t, err)
	assert.Equal(t, []byte("supervised output"), result.Stdout)
	assert.Equal(t, result.Stdout, result.Output)
}

func TestSupervisedRunnerLocalIPCScope(t *testing.T) {
	runner, err := newNativeRunner()
	require.NoError(t, err)
	localIPC := sandboxTestRequest(t, buildSandboxHelper(t, "local-ipc", "", "result.bin"), []byte("input"), 1<<20)
	localIPC.Policy.AllowLocalIPC = true
	localIPC.Policy.Supervision = Supervision{
		Mode: SupervisedFileMode, InputName: "source.docx", OutputName: "result.bin",
		WorkBytes: 1 << 20, MaxOutputBytes: 1 << 20,
	}
	result, err := runner.Run(t.Context(), localIPC)
	skipUnavailableSandbox(t, err)
	require.NoError(t, err)
	assert.Equal(t, []byte("allowed"), result.Output)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	network := sandboxTestRequest(t, buildSandboxHelper(t, "file-network", listener.Addr().String(), "result.bin"), []byte("input"), 1<<20)
	network.Policy.AllowLocalIPC = true
	network.Policy.Supervision = localIPC.Policy.Supervision
	result, err = runner.Run(t.Context(), network)
	skipUnavailableSandbox(t, err)
	require.NoError(t, err)
	assert.Equal(t, []byte("denied"), result.Output)
}

func TestSupervisedRunnerRejectsOutputOverflow(t *testing.T) {
	executable := buildSandboxHelper(t, "file-overflow", "", "result.bin")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("input"), 1024)
	request.Policy.Supervision = Supervision{
		Mode: SupervisedFileMode, InputName: "source.docx", OutputName: "result.bin",
		WorkBytes: 1 << 20, MaxOutputBytes: 1024,
	}
	result, err := runner.Run(t.Context(), request)
	skipUnavailableSandbox(t, err)
	require.ErrorIs(t, err, ErrOutputTooLarge)
	assert.LessOrEqual(t, int64(len(result.Stdout)), request.Policy.MaxStdoutBytes)
}

func TestSupervisedRunnerReapsAdoptedDescendantAfterDirectExit(t *testing.T) {
	executable := buildSandboxHelper(t, "file-descendant-exit", "", "result.bin")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("input"), 1<<20)
	request.Policy.AllowLocalIPC = true
	request.Policy.Supervision = Supervision{
		Mode: SupervisedFileMode, InputName: "source.docx", OutputName: "result.bin",
		WorkBytes: 1 << 20, MaxOutputBytes: 1 << 20,
	}
	started := time.Now()
	result, err := runner.Run(t.Context(), request)
	skipUnavailableSandbox(t, err)
	require.NoError(t, err)
	assert.Equal(t, []byte("supervised output"), result.Output)
	assert.Less(t, time.Since(started), 2*time.Second)
}

func TestSupervisedRunnerNeverLaunchesReplacementExecutable(t *testing.T) {
	executable := buildSandboxHelper(t, "file-output", "", "result.bin")
	replacement := buildSandboxHelper(t, "exit-125", "", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("input"), 1<<20)
	request.Policy.Supervision = Supervision{
		Mode: SupervisedFileMode, InputName: "source.docx", OutputName: "result.bin",
		WorkBytes: 1 << 20, MaxOutputBytes: 1 << 20,
	}
	require.NoError(t, os.Rename(replacement, executable))
	_, err = runner.Run(t.Context(), request)
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestNativeRunnerCancellationReapsDescendantProcessTree(t *testing.T) {
	executable := buildSandboxHelper(t, "descendant", "", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("descendant probe"), 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan sandboxRunOutcome, 1)
	started := time.Now()
	go func() {
		result, runErr := runner.Run(ctx, request)
		finished <- sandboxRunOutcome{result: result, err: runErr}
	}()
	select {
	case outcome := <-finished:
		skipUnavailableSandbox(t, outcome.err)
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
	executable := buildSandboxHelper(t, "overflow", "", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("overflow probe"), 1024)
	started := time.Now()
	result, err := runner.Run(t.Context(), request)
	skipUnavailableSandbox(t, err)
	require.ErrorIs(t, err, ErrOutputTooLarge)
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.LessOrEqual(t, int64(len(result.Stdout)), request.Policy.MaxStdoutBytes)
}

func TestNativeRunnerDoesNotMisclassifyBridgeExit125AsLauncherFailure(t *testing.T) {
	executable := buildSandboxHelper(t, "exit-125", "", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("exit probe"), 1<<20)
	_, err = runner.Run(t.Context(), request)
	skipUnavailableSandbox(t, err)
	require.ErrorIs(t, err, ErrChildFailed)
	require.NotErrorIs(t, err, ErrUnavailable)
}

func TestNativeRunnerHonorsCancellationBeforeExecutablePreparation(t *testing.T) {
	executable := buildSandboxHelper(t, "echo", "", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("canceled probe"), 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = runner.Run(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
}

func TestNativeRunnerNeverLaunchesExecutableContentOutsidePinnedDigest(t *testing.T) {
	executable := buildSandboxHelper(t, "echo", "", "")
	replacement := buildSandboxHelper(t, "replacement", "", "")
	runner, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("identity probe"), 1<<20)
	require.NoError(t, os.Rename(replacement, executable))
	_, err = runner.Run(t.Context(), request)
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestVerifiedExecutableContentCannotChangeAfterDigestVerification(t *testing.T) {
	executable := buildSandboxHelper(t, "echo", "", "")
	replacement := buildSandboxHelper(t, "replacement", "", "")
	_, err := newNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("identity probe"), 1<<20)
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

func sandboxTestRequest(t *testing.T, executable string, stdin []byte, maxStdout int64) Request {
	t.Helper()
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	stdinDigest := sha256.Sum256(stdin)
	return Request{
		PolicyFingerprint: "sandbox-test-policy",
		Policy: Policy{
			Executable: executable, ExecutableSHA256: hex.EncodeToString(digest[:]),
			Arguments: []string{"--protocol", "sandbox"}, Environment: cleanSandboxEnvironment(),
			Directory: filepath.Dir(executable), MaxStdinBytes: max(int64(len(stdin)), 1),
			MaxStdoutBytes: maxStdout, WorkBytes: 1 << 20,
		},
		Stdin: stdin, StdinSHA256: hex.EncodeToString(stdinDigest[:]),
	}
}

func buildSandboxHelper(t *testing.T, mode, networkAddress, outputName string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "sandbox-helper")
	ldflags := strings.Join([]string{
		"-X=main.mode=" + mode,
		"-X=main.networkAddress=" + networkAddress,
		"-X=main.outputName=" + outputName,
	}, " ")
	command := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags,
		"-o", target, "./testdata/sandboxhelper")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return target
}

func cleanSandboxEnvironment() []string {
	return []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC"}
}

func skipUnavailableSandbox(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("native Linux namespace isolation unavailable: %v", err)
	}
}

func evaluateSeccomp(t *testing.T, filters []unix.SockFilter, syscallNumber uint32) uint32 {
	t.Helper()
	return evaluateSeccompDomain(t, filters, syscallNumber, unix.AF_UNIX)
}

func evaluateSeccompDomain(t *testing.T, filters []unix.SockFilter, syscallNumber, domain uint32) uint32 {
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
			case 16:
				accumulator = domain
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
