//go:build linux

package isolate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	for _, protocol := range []string{"docbank-trafilatura/v2", "docbank-pymupdf/v1"} {
		t.Run(protocol, func(t *testing.T) {
			stdin := []byte("exact supplied bytes\x00remain data, never arguments")
			request := nativeTestRequest(t, runner, executable, stdin, 1<<20)

			request.Arguments[1] = protocol
			request.PolicyFingerprint = RequestPolicyFingerprint(runner.Identity(), request)
			result, err := runner.Run(t.Context(), request)
			skipUnavailableNativeIsolation(t, err)
			require.NoError(t, err)

			var response struct {
				Executable   string            `json:"executable"`
				PID          int               `json:"pid"`
				Status       string            `json:"status"`
				ProcReadOnly bool              `json:"proc_read_only"`
				Namespaces   map[string]string `json:"namespaces"`
				Arguments    []string          `json:"arguments"`
				Environment  []string          `json:"environment"`
				StdinSHA256  string            `json:"stdin_sha256"`
			}
			require.NoError(t, json.Unmarshal(result.Stdout, &response))
			digest := sha256.Sum256(stdin)
			assert.Equal(t, hex.EncodeToString(digest[:]), response.StdinSHA256)
			assert.Equal(t, []string{"--protocol", protocol}, response.Arguments)
			assert.Equal(t, nativeEnvironment(), response.Environment)
			assert.Equal(t, nativeRunnerIdentity, result.Attestation.RunnerIdentity)
			assert.True(t, result.Attestation.NetworkDisabled)
			assert.True(t, result.Attestation.ProcessTreeContained)
			assert.True(t, result.Attestation.DigestVerifiedLaunch)
			assert.Equal(t, executable, response.Executable)
			assert.Equal(t, 1, response.PID)
			assert.Contains(t, response.Status, "NoNewPrivs:\t1")
			assert.Contains(t, response.Status, "Seccomp:\t2")
			assert.True(t, response.ProcReadOnly)
			for _, name := range []string{"user", "net", "pid", "mnt"} {
				parent, err := os.Readlink("/proc/self/ns/" + name)
				require.NoError(t, err)
				require.NotEmpty(t, response.Namespaces[name])
				assert.NotEqual(t, parent, response.Namespaces[name])
			}
		})
	}
}

func TestNativeRunnerDeniesLoopbackNetworkAccess(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	executable := buildIsolatedHelper(t, "network", listener.Addr().String())
	runner, err := NewNativeRunner()
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
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("unix network probe"), 1<<20)

	result, err := runner.Run(t.Context(), request)
	skipUnavailableNativeIsolation(t, err)
	require.NoError(t, err)
	assert.Equal(t, "denied", string(result.Stdout))
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
	request := IsolatedRunRequest{Executable: "/opt/synthetic-bridge", Arguments: []string{"--protocol", "docbank-pymupdf/v1"}, Environment: nativeEnvironment()}
	control, token, err := openNativeLaunchControl(request)
	require.NoError(t, err)
	defer func() { _ = control.Close() }()
	arguments := []string{"/proc/self/exe", nativeLauncherMarker, token}
	record, authenticated := authenticatedNativeLaunch(arguments, int(control.Fd()))
	require.True(t, authenticated)
	assert.Equal(t, request.Executable, record.Executable)
	assert.Equal(t, request.Arguments, record.Arguments)
	assert.Equal(t, request.Environment, record.Environment)
	_, authenticated = authenticatedNativeLaunch(append(arguments, "/opt/substitution"), int(control.Fd()))
	assert.False(t, authenticated)
	_, authenticated = authenticatedNativeLaunch(arguments, -1)
	assert.False(t, authenticated)
	arguments[2] = strings.Repeat("0", nativeLauncherTokenBytes*2)
	_, authenticated = authenticatedNativeLaunch(arguments, int(control.Fd()))
	assert.False(t, authenticated)
}

func TestNativeRunnerCancellationReapsDescendantProcessTree(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "descendant.lock")
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	executable := buildIsolatedHelper(t, "descendant", lockPath)
	runner, err := NewNativeRunner()
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

	observeDescendantLockHeld(t, lockPath, finished)
	cancel()
	outcome := <-finished
	require.ErrorIs(t, outcome.err, context.Canceled)
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.True(t, outcome.result.Attestation.ProcessTreeContained)
	require.Eventually(t, func() bool { return exclusiveLockAvailable(t, lockPath) },
		time.Second, 10*time.Millisecond, "descendant must release its inherited host lock")
}

func TestNativeRunnerTerminatesPromptlyOnStdoutOverflow(t *testing.T) {
	executable := buildIsolatedHelper(t, "overflow", "")
	runner, err := NewNativeRunner()
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
	runner, err := NewNativeRunner()
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
	runner, err := NewNativeRunner()
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
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	request := nativeTestRequest(t, runner, executable, []byte("identity probe"), 1<<20)
	require.NoError(t, os.Rename(replacement, executable))

	_, err = runner.Run(t.Context(), request)
	require.ErrorIs(t, err, ErrIsolationUnavailable)
}

func TestVerifiedExecutableContentCannotChangeAfterDigestVerification(t *testing.T) {
	executable := buildIsolatedHelper(t, "echo", "")
	replacement := buildIsolatedHelper(t, "replacement", "")
	runner, err := NewNativeRunner()
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

func nativeTestRequest(
	t *testing.T, runner IsolatedRunner, executable string, stdin []byte, maxStdout int64,
) IsolatedRunRequest {
	t.Helper()
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	stdinDigest := sha256.Sum256(stdin)
	request := IsolatedRunRequest{Executable: executable, ExecutableSHA256: hex.EncodeToString(digest[:]),
		Arguments: []string{"--protocol", "docbank-trafilatura/v2"}, Environment: nativeEnvironment(), Directory: filepath.Dir(executable),
		Stdin: stdin, StdinSHA256: hex.EncodeToString(stdinDigest[:]), MaxStdoutBytes: maxStdout,
		Requirements: IsolationRequirements{NetworkDisabled: true, KillProcessTree: true, VerifyExecutableSHA256: true}}
	request.PolicyFingerprint = RequestPolicyFingerprint(runner.Identity(), request)
	return request
}

func buildIsolatedHelper(t *testing.T, mode, networkAddress string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "isolated-helper")
	ldflags := strings.Join([]string{
		"-X=main.mode=" + mode,
		"-X=main.networkAddress=" + networkAddress,
	}, " ")
	command := exec.Command("go", "build", "-tags", "fts5", "-trimpath", "-ldflags", ldflags,
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

func observeDescendantLockHeld(
	t *testing.T, lockPath string, finished <-chan nativeRunOutcome,
) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case outcome := <-finished:
			skipUnavailableNativeIsolation(t, outcome.err)
			require.FailNow(t, "isolated runner exited before descendant held its lock", "%v", outcome.err)
		case <-ticker.C:
			owner, err := os.ReadFile(lockPath)
			require.NoError(t, err)
			if !exclusiveLockAvailable(t, lockPath) && string(owner) == "descendant-ready\n" {
				return
			}
		case <-deadline.C:
			require.FailNow(t, "descendant did not acquire its host-visible lock")
		}
	}
}

func exclusiveLockAvailable(t *testing.T, lockPath string) bool {
	t.Helper()
	file, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false
	}
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(file.Fd()), unix.LOCK_UN))
	return true
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

func TestNativeLauncherRejectsMalformedControl(t *testing.T) {
	record := nativeLaunchRecord{Token: strings.Repeat("a", 64), Executable: "/opt/synthetic-bridge", Arguments: []string{"--protocol", "docbank-pymupdf/v1"}, Environment: nativeEnvironment(), ExecutableFD: 3, ControlFD: 4, StatusFD: 5}
	for _, test := range []struct {
		name     string
		mutate   func(*nativeLaunchRecord)
		suffix   string
		truncate bool
		unsealed bool
	}{
		{name: "unsealed", unsealed: true},
		{name: "truncated", truncate: true},
		{name: "trailing data", suffix: "{}"},
		{name: "forged token", mutate: func(r *nativeLaunchRecord) { r.Token = strings.Repeat("b", 64) }},
		{name: "wrong descriptor", mutate: func(r *nativeLaunchRecord) { r.ExecutableFD = 6 }},
		{name: "wrong control descriptor", mutate: func(r *nativeLaunchRecord) { r.ControlFD = 6 }},
		{name: "wrong status descriptor", mutate: func(r *nativeLaunchRecord) { r.StatusFD = 6 }},
		{name: "unknown protocol", mutate: func(r *nativeLaunchRecord) { r.Arguments = []string{"--protocol", "unknown"} }},
		{name: "extra argument", mutate: func(r *nativeLaunchRecord) { r.Arguments = []string{"--protocol", "docbank-pymupdf/v1", "extra"} }},
		{name: "changed environment", mutate: func(r *nativeLaunchRecord) { r.Environment = []string{"TZ=elsewhere"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := record
			if test.mutate != nil {
				test.mutate(&value)
			}
			encoded, err := json.Marshal(value)
			require.NoError(t, err)
			if test.truncate {
				encoded = encoded[:len(encoded)/2]
			}
			encoded = append(encoded, test.suffix...)
			fd, err := unix.MemfdCreate("synthetic-control", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
			require.NoError(t, err)
			defer func() { _ = unix.Close(fd) }()
			_, err = unix.Write(fd, encoded)
			require.NoError(t, err)
			if !test.unsealed {
				_, err = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL)
				require.NoError(t, err)
			}
			_, authenticated := authenticatedNativeLaunch([]string{"/proc/self/exe", nativeLauncherMarker, record.Token}, fd)
			assert.False(t, authenticated)
		})
	}
}

func TestNativeRunnerParentDeathReapsDescendants(t *testing.T) {
	const childEnv = "DOCBANK_TEST_ISOLATE_PARENT"
	if executable := os.Getenv(childEnv); executable != "" {
		runner, err := NewNativeRunner()
		require.NoError(t, err)
		_, err = runner.Run(t.Context(), nativeTestRequest(t, runner, executable, []byte("parent death probe"), 1024))
		if errors.Is(err, ErrIsolationUnavailable) {
			os.Exit(77)
		}
		require.NoError(t, err)
		return
	}
	lockPath := filepath.Join(t.TempDir(), "descendant.lock")
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	executable := buildIsolatedHelper(t, "descendant", lockPath)
	parent := exec.Command(os.Args[0], "-test.run=^TestNativeRunnerParentDeathReapsDescendants$") //nolint:gosec // The current test executable owns the synthetic native runner.
	parent.Env = append(os.Environ(), childEnv+"="+executable)
	require.NoError(t, parent.Start())
	finished := make(chan nativeRunOutcome, 1)
	go func() {
		err := parent.Wait()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 77 {
			err = ErrIsolationUnavailable
		}
		finished <- nativeRunOutcome{err: err}
	}()
	defer func() { _ = parent.Process.Kill() }()
	observeDescendantLockHeld(t, lockPath, finished)
	require.NoError(t, parent.Process.Kill())
	<-finished
	require.Eventually(t, func() bool { return exclusiveLockAvailable(t, lockPath) }, 2*time.Second, 10*time.Millisecond)
}
