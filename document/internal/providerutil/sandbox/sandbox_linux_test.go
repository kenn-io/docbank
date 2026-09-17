//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestNativeRunnerUsesExactStdinArgumentsAndCleanEnvironment(t *testing.T) {
	executable := buildSandboxHelper(t, "echo", "", "")
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	stdin := []byte("exact supplied bytes\x00remain data, never arguments")
	result, err := runner.Run(t.Context(), sandboxTestRequest(t, executable, stdin, 1<<20))
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
	assert.Equal(t, "strict-exec", result.Attestation.FilesystemMode)
	assert.False(t, result.Attestation.PrivateRootInstalled)
}

func TestStrictExecProvidesPrivateProcAndTemporaryDirectory(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	result, err := runner.Run(t.Context(), sandboxTestRequest(t,
		buildSandboxHelper(t, "strict-fs", "", ""), []byte("probe"), 1<<20))
	require.NoError(t, err)
	t.Logf("strict filesystem probe: %s", result.Output)
	assert.Contains(t, string(result.Output), "proc=true;tmp=true")
}

func TestPrivateProfileSchemaMatchesInstalledRegistry(t *testing.T) {
	var installed []byte
	for _, path := range []string{
		"/etc/libreoffice/registry/main.xcd",
		"/usr/lib/libreoffice/share/registry/main.xcd",
		"/usr/lib/libreoffice/share/.registry/main.xcd",
	} {
		data, err := os.ReadFile(path)
		if err == nil {
			installed = data
			break
		}
	}
	require.NotEmpty(t, installed)
	text := string(installed)
	assert.Contains(t, text, `<group oor:name="Security">`)
	assert.Contains(t, text, `<group oor:name="Scripting">`)
	for _, name := range []string{
		"MacroSecurityLevel", "DisableMacrosExecution", "DisableActiveContent", "BlockUntrustedRefererLinks",
	} {
		assert.Contains(t, text, `oor:name="`+name+`"`)
	}
	settings := privateProfileSettings()
	assert.Contains(t, settings, `oor:path="/org.openoffice.Office.Common/Security/Scripting"`)
	assert.Contains(t, settings, `oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value>`)
	assert.Contains(t, settings, `oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value>`)
	assert.Contains(t, settings, `oor:name="DisableActiveContent" oor:op="fuse"><value>true</value>`)
	assert.Contains(t, settings, `oor:name="BlockUntrustedRefererLinks" oor:op="fuse"><value>true</value>`)
	assert.NotContains(t, settings, "UpdateDocMode")
	t.Log("main.xcd Security/Scripting properties and generated registry path/values match; UpdateDocMode absent")
}

func TestAuthenticatedLaunchChecksTokenSealFstatAndExecutableBinding(t *testing.T) {
	request := sandboxTestRequest(t, buildSandboxHelper(t, "echo", "", ""), []byte("input"), 1<<20)
	control, token, err := openLaunchControl(request)
	require.NoError(t, err)
	defer func() { _ = control.Close() }()
	arguments := []string{"/proc/self/exe", launcherMarker, token, request.Policy.Executable}
	_, authenticated := authenticatedLaunch(arguments, int(control.Fd()))
	assert.True(t, authenticated)
	badToken := append([]string(nil), arguments...)
	badToken[2] = strings.Repeat("0", launcherTokenBytes*2)
	_, authenticated = authenticatedLaunch(badToken, int(control.Fd()))
	assert.False(t, authenticated)
	badExecutable := append([]string(nil), arguments...)
	badExecutable[3] = filepath.Join(filepath.Dir(request.Policy.Executable), "other")
	_, authenticated = authenticatedLaunch(badExecutable, int(control.Fd()))
	assert.False(t, authenticated)
	_, authenticated = authenticatedLaunch(arguments, -1)
	assert.False(t, authenticated)
	unsealedFD, err := unix.MemfdCreate("unsealed", unix.MFD_CLOEXEC)
	require.NoError(t, err)
	defer func() { _ = unix.Close(unsealedFD) }()
	_, authenticated = authenticatedLaunch(arguments, unsealedFD)
	assert.False(t, authenticated)
}

func TestLauncherRejectsOutOfBandStatus(t *testing.T) {
	assert.False(t, launcherReadyStatusRead(bytes.NewReader([]byte{launcherFailureStatus})))
	assert.True(t, launcherReadyStatusRead(bytes.NewReader([]byte{launcherReadyStatus})))
}

func TestLauncherStatusProtocolIsModeSpecific(t *testing.T) {
	status := launcherStatusRead(bytes.NewReader([]byte{launcherReadyStatus}))
	assert.True(t, status.ready)
	assert.Zero(t, status.detail)
	assert.Zero(t, status.restarts)

	status = launcherStatusRead(bytes.NewReader([]byte{launcherReadyStatus, launcherRestartStatusBase + 1}))
	assert.True(t, status.ready)
	assert.Equal(t, 1, status.restarts)

	status = launcherStatusRead(bytes.NewReader([]byte{launcherReadyStatus, launcherFailureStatus}))
	assert.True(t, status.ready)
	assert.Equal(t, launcherFailureStatus, status.detail)
	status = launcherStatusRead(bytes.NewReader([]byte{launcherReadyStatus, launcherChildFailureStatus}))
	assert.Equal(t, launcherChildFailureStatus, status.detail)
	status = launcherStatusRead(bytes.NewReader([]byte{launcherReadyStatus, launcherOutputFailureStatus}))
	assert.Equal(t, launcherOutputFailureStatus, status.detail)
}

func TestNativeRunnerDeniesLoopbackNetworkAccess(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	result, err := runner.Run(t.Context(), sandboxTestRequest(t,
		buildSandboxHelper(t, "network", listener.Addr().String(), ""), []byte("probe"), 1<<20))
	require.NoError(t, err)
	assert.Equal(t, "denied", string(result.Stdout))
}

func TestNativeRunnerDeniesHostPathnameUnixSocketAccess(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	result, err := runner.Run(t.Context(), sandboxTestRequest(t,
		buildSandboxHelper(t, "unix-network", socketPath, ""), []byte("probe"), 1<<20))
	require.NoError(t, err)
	assert.Equal(t, "denied", string(result.Stdout))
}

func TestPrivateRootDeniesHostPathnameUnixSockets(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "host.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	result, err := runner.Run(t.Context(), privateTestRequest(t,
		buildSandboxHelper(t, "file-unix-network", socketPath, "result.bin")))
	require.NoError(t, err)
	assert.Equal(t, []byte("denied"), result.Output)
}

func TestPrivateRootDeniesSocketInsertedAfterRuntimeDiscovery(t *testing.T) {
	runtimeDir := t.TempDir()
	declaredPath := filepath.Join(runtimeDir, "declared")
	declared := []byte("declared runtime")
	require.NoError(t, os.WriteFile(declaredPath, declared, 0o600))
	digest := sha256.Sum256(declared)
	// This manifest represents the regular file discovered before the socket exists.
	entry := RuntimeFile{SourcePath: declaredPath, GuestPath: "/usr/runtime/socket-dir/declared", SHA256: hex.EncodeToString(digest[:])}
	socketPath := filepath.Join(runtimeDir, "inserted.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	request := privateTestRequest(t, buildSandboxHelper(t, "file-unix-network", "/usr/runtime/socket-dir/inserted.sock", "result.bin"))
	request.Policy.PrivateRoot.Runtime = []RuntimeFile{entry}
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	result, err := runner.Run(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, []byte("denied"), result.Output)
}

func TestNativeRunnerCannotReadOrModifyHostFiles(t *testing.T) {
	hostPath := filepath.Join(t.TempDir(), "host-only.txt")
	require.NoError(t, os.WriteFile(hostPath, []byte("host-only"), 0o600))
	before, err := os.Stat(hostPath)
	require.NoError(t, err)
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	result, err := runner.Run(t.Context(), sandboxTestRequest(t,
		buildSandboxHelper(t, "host-file", hostPath, ""), []byte("probe"), 1<<20))
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

func TestExecPolicyMatchesBaselineDenyListAndPaths(t *testing.T) {
	assert.Equal(t, []string{"/usr", "/lib", "/lib64", "/bin", "/proc"}, execLandlockPaths())
	assert.Equal(t, []string{rootPath("etc", "ld.so.cache"), rootPath("etc", "localtime")}, execLandlockFiles())
	filters, err := execNetworkFilters(unix.AUDIT_ARCH_X86_64)
	require.NoError(t, err)
	denied := unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	for _, syscallNumber := range blockedNetworkSyscalls() {
		assert.Equal(t, denied, evaluateSeccomp(t, filters, uint32(syscallNumber)))
	}
}

func TestPrivateRootAllowsOnlyUnixSockets(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	result, err := runner.Run(t.Context(), privateTestRequest(t,
		buildSandboxHelper(t, "local-ipc", "", "result.bin")))
	require.NoError(t, err)
	assert.Equal(t, []byte("allowed"), result.Output)
	assert.True(t, result.Attestation.PrivateRootInstalled)
	assert.Equal(t, "sha256:"+strings.Repeat("0", 64), result.Attestation.RuntimeIdentity)
	assert.True(t, result.Attestation.UnixIPCAllowed)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	result, err = runner.Run(t.Context(), privateTestRequest(t,
		buildSandboxHelper(t, "file-network", listener.Addr().String(), "result.bin")))
	require.NoError(t, err)
	assert.Equal(t, []byte("denied"), result.Output)

	hostPath := filepath.Join(t.TempDir(), "host-only.txt")
	require.NoError(t, os.WriteFile(hostPath, []byte("host-only"), 0o600))
	result, err = runner.Run(t.Context(), privateTestRequest(t,
		buildSandboxHelper(t, "file-host", hostPath, "result.bin")))
	require.NoError(t, err)
	assert.Equal(t, []byte("denied"), result.Output)
}

func TestSupervisedRunnerReapsAdoptedDescendantAfterDirectExit(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	started := time.Now()
	result, err := runner.Run(t.Context(), privateTestRequest(t,
		buildSandboxHelper(t, "file-descendant-exit", "", "result.bin")))
	require.NoError(t, err)
	t.Log("descendant readiness handshake completed before direct parent exit; Run returned after reaping")
	assert.Equal(t, []byte("supervised output"), result.Output)
	assert.Less(t, time.Since(started), 2*time.Second)
}

func TestSupervisedRunnerRetriesExit81Once(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	result, err := runner.Run(t.Context(), privateTestRequest(t,
		buildSandboxHelper(t, "file-exit81-once", "", "result.bin")))
	require.NoError(t, err)
	assert.Equal(t, []byte("retried output"), result.Output)
	assert.Equal(t, 1, result.Attestation.RestartCount)
}

func TestSupervisedRunnerPassesOnlyPolicyArguments(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	request := privateTestRequest(t, buildSandboxHelper(t, "argv", "", "result.bin"))
	request.Policy.Arguments = []string{"--alpha", "beta"}
	result, err := runner.Run(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, "--alpha\x00beta", string(result.Output))
}

func TestPrivateRootStagesRuntimeFilesAndMountModes(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	request := privateTestRequest(t, buildSandboxHelper(t, "runtime-mounts", "", "result.bin"))
	runtimeDir := t.TempDir()
	executablePath := filepath.Join(runtimeDir, "exec")
	dataPath := filepath.Join(runtimeDir, "data")
	executableBytes := []byte("exec runtime")
	dataBytes := []byte("data runtime")
	require.NoError(t, os.WriteFile(executablePath, executableBytes, 0o644))
	require.NoError(t, os.WriteFile(dataPath, dataBytes, 0o644))
	executableDigest := sha256.Sum256(executableBytes)
	dataDigest := sha256.Sum256(dataBytes)
	request.Policy.Arguments = []string{"--runtime-mounts", "/usr/runtime/exec", "/usr/runtime/data", "/usr/runtime/link"}
	request.Policy.PrivateRoot.Runtime = []RuntimeFile{
		{SourcePath: dataPath, GuestPath: "/usr/runtime/data", SHA256: hex.EncodeToString(dataDigest[:])},
		{SourcePath: executablePath, GuestPath: "/usr/runtime/exec", SHA256: hex.EncodeToString(executableDigest[:]), Executable: true},
	}
	request.Policy.PrivateRoot.Symlinks = []RuntimeSymlink{{GuestPath: "/usr/runtime/link", Target: "data"}}
	result, err := runner.Run(t.Context(), request)
	require.NoError(t, err)
	assert.Contains(t, string(result.Output), "exec=true,data=false,link=false")
	assert.Contains(t, string(result.Output), ";exec-content=exec runtime;data-content=data runtime")
}

func TestPrivateRootClosesRuntimeDescriptorsBeforeRendererExec(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	request := privateTestRequest(t, buildSandboxHelper(t, "fd-count", "", "result.bin"))
	runtimePath := filepath.Join(t.TempDir(), "runtime")
	content := []byte("runtime")
	require.NoError(t, os.WriteFile(runtimePath, content, 0o644))
	digest := sha256.Sum256(content)
	request.Policy.PrivateRoot.Runtime = []RuntimeFile{{
		SourcePath: runtimePath, GuestPath: "/usr/runtime/data", SHA256: hex.EncodeToString(digest[:]),
	}}
	result, err := runner.Run(t.Context(), request)
	require.NoError(t, err)
	count, err := strconv.Atoi(string(result.Output))
	require.NoError(t, err)
	assert.LessOrEqual(t, count, 8)
}

func TestSupervisedRunnerClassifiesChildExit125(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	_, err = runner.Run(t.Context(), privateTestRequest(t,
		buildSandboxHelper(t, "exit-125", "", "result.bin")))
	require.ErrorIs(t, err, ErrChildFailed)
}

func TestSupervisedRunnerClassifiesChildExit124(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	_, err = runner.Run(t.Context(), privateTestRequest(t,
		buildSandboxHelper(t, "exit-124", "", "result.bin")))
	require.ErrorIs(t, err, ErrChildFailed)
}

func TestSupervisedRunnerClassifiesOutputOverflowSeparately(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	request := privateTestRequest(t, buildSandboxHelper(t, "file-overflow", "", "result.bin"))
	request.Policy.PrivateRoot.MaxOutputBytes = 1 << 10
	_, err = runner.Run(t.Context(), request)
	require.ErrorIs(t, err, ErrOutputTooLarge)
}

func TestStrictExecClassifiesChildExit124AsFailure(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	_, err = runner.Run(t.Context(), sandboxTestRequest(t,
		buildSandboxHelper(t, "exit-124", "", ""), []byte("probe"), 1<<20))
	require.ErrorIs(t, err, ErrChildFailed)
}

func TestStrictExecFailureAfterReadyIsUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-an-executable")
	require.NoError(t, os.WriteFile(path, []byte("not an executable"), 0o700))
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	_, err = runner.Run(t.Context(), sandboxTestRequest(t, path, []byte("probe"), 1<<20))
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestNativeRunnerCancellationBeforePreparation(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = runner.Run(ctx, sandboxTestRequest(t,
		buildSandboxHelper(t, "echo", "", ""), []byte("input"), 1<<20))
	require.ErrorIs(t, err, ErrCanceledBeforeLaunch)
}

func TestNativeRunnerCancellationReapsDescendantProcessTree(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, buildSandboxHelper(t, "descendant", "", ""), []byte("probe"), 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, runErr := runner.Run(ctx, request); finished <- runErr }()
	select {
	case err := <-finished:
		require.FailNow(t, "isolated runner exited before cancellation", "%v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	require.ErrorIs(t, <-finished, context.Canceled)
}

func TestNativeRunnerTerminatesPromptlyOnStdoutOverflow(t *testing.T) {
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	started := time.Now()
	result, err := runner.Run(t.Context(), sandboxTestRequest(t,
		buildSandboxHelper(t, "overflow", "", ""), []byte("probe"), 1024))
	require.ErrorIs(t, err, ErrOutputTooLarge)
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.LessOrEqual(t, int64(len(result.Stdout)), int64(1024))
}

func TestNativeRunnerNeverLaunchesExecutableContentOutsidePinnedDigest(t *testing.T) {
	executable := buildSandboxHelper(t, "echo", "", "")
	replacement := buildSandboxHelper(t, "replacement", "", "")
	runner, err := NewNativeRunner()
	require.NoError(t, err)
	request := sandboxTestRequest(t, executable, []byte("probe"), 1<<20)
	require.NoError(t, os.Rename(replacement, executable))
	_, err = runner.Run(t.Context(), request)
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestVerifiedExecutableContentCannotChangeAfterDigestVerification(t *testing.T) {
	executable := buildSandboxHelper(t, "echo", "", "")
	replacement := buildSandboxHelper(t, "replacement", "", "")
	request := sandboxTestRequest(t, executable, []byte("probe"), 1<<20)
	prepared, err := openVerifiedExecutable(t.Context(), request)
	require.NoError(t, err)
	defer func() { _ = prepared.Close() }()
	want, err := os.ReadFile(executable)
	require.NoError(t, err)
	replacementBytes, err := os.ReadFile(replacement)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(executable, replacementBytes, 0o700))
	got, err := os.ReadFile("/proc/self/fd/" + strconv.FormatUint(uint64(prepared.Fd()), 10))
	require.NoError(t, err)
	assert.Equal(t, sha256.Sum256(want), sha256.Sum256(got))
}

func TestSeccompFilterDeniesX32ABI(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x32 ABI exists only on amd64")
	}
	filters, err := execNetworkFilters(unix.AUDIT_ARCH_X86_64)
	require.NoError(t, err)
	denied := unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	for _, syscallNumber := range []uint32{unix.SYS_SOCKET, unix.SYS_CONNECT, unix.SYS_IO_URING_SETUP} {
		assert.Equal(t, denied, evaluateSeccomp(t, filters, syscallNumber))
		assert.Equal(t, denied, evaluateSeccomp(t, filters, syscallNumber|nativeX32SyscallBit))
	}
}

func TestPrivateRootUnixSocketFilters(t *testing.T) {
	filters, err := unixOnlyNetworkFilters(unix.AUDIT_ARCH_X86_64)
	require.NoError(t, err)
	denied := unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	for _, domain := range []uint32{unix.AF_INET, unix.AF_INET6, unix.AF_NETLINK} {
		assert.Equal(t, denied, evaluateSeccompDomain(t, filters, uint32(unix.SYS_SOCKET), domain))
		assert.Equal(t, denied, evaluateSeccompDomain(t, filters, uint32(unix.SYS_SOCKETPAIR), domain))
	}
	assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW), evaluateSeccompDomain(t, filters, uint32(unix.SYS_SOCKET), unix.AF_UNIX))
	assert.Equal(t, uint32(unix.SECCOMP_RET_ALLOW), evaluateSeccompDomain(t, filters, uint32(unix.SYS_SOCKETPAIR), unix.AF_UNIX))
	assert.Equal(t, denied, evaluateSeccompDomain(t, filters, uint32(unix.SYS_SENDMSG), unix.AF_UNIX))
}

func TestPrivateRootRejectsRuntimeEntryCeilingAtPolicyOwner(t *testing.T) {
	root := &PrivateRoot{
		Runtime:         make([]RuntimeFile, MaxRuntimeEntries+1),
		RuntimeIdentity: "sha256:" + strings.Repeat("a", 64),
		WorkBytes:       1, InputName: "input", OutputName: "output", MaxOutputBytes: 1,
	}
	policy := Policy{
		Mode: SupervisedFileMode, Executable: "/renderer", ExecutableSHA256: strings.Repeat("b", 64),
		Arguments: []string{"renderer"}, Environment: []string{"LANG=C"},
		MaxStdinBytes: 1, MaxStdoutBytes: 1, PrivateRoot: root,
	}
	require.ErrorContains(t, policy.Validate(), "entry count")
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
			Mode: ExecMode, Executable: executable, ExecutableSHA256: hex.EncodeToString(digest[:]),
			Arguments: []string{"--protocol", "sandbox"}, Environment: cleanSandboxEnvironment(),
			Directory: filepath.Dir(executable), MaxStdinBytes: max(int64(len(stdin)), 1),
			MaxStdoutBytes: maxStdout,
		},
		Stdin: stdin, StdinSHA256: hex.EncodeToString(stdinDigest[:]),
	}
}

func privateTestRequest(t *testing.T, executable string) Request {
	t.Helper()
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	file, err := os.CreateTemp(".", ".sandbox-private-helper-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(file.Name()) })
	_, err = file.Write(data)
	require.NoError(t, err)
	require.NoError(t, file.Chmod(0o700))
	require.NoError(t, file.Close())
	target, err := filepath.Abs(file.Name())
	require.NoError(t, err)
	request := sandboxTestRequest(t, target, []byte("input"), 1<<20)
	request.Policy.Mode = SupervisedFileMode
	request.Policy.PrivateRoot = &PrivateRoot{
		RuntimeIdentity: "sha256:" + strings.Repeat("0", 64),
		WorkBytes:       64 << 20, InputName: "source.docx", OutputName: "result.bin",
		MaxOutputBytes: 1 << 20, UnixIPC: true,
	}
	return request
}

func buildSandboxHelper(t *testing.T, mode, networkAddress, outputName string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "sandbox-helper")
	ldflags := strings.Join([]string{
		"-X=main.mode=" + mode, "-X=main.networkAddress=" + networkAddress,
		"-X=main.outputName=" + outputName,
	}, " ")
	command := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags,
		"-o", target, "./testdata/sandboxhelper")
	environment := os.Environ()
	set := false
	for index, entry := range environment {
		if strings.HasPrefix(entry, "CGO_ENABLED=") {
			environment[index] = "CGO_ENABLED=0"
			set = true
		}
	}
	if !set {
		environment = append(environment, "CGO_ENABLED=0")
	}
	command.Env = environment
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return target
}

func cleanSandboxEnvironment() []string {
	return []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC"}
}

func evaluateSeccomp(t *testing.T, filters []unix.SockFilter, syscallNumber uint32) uint32 {
	t.Helper()
	return evaluateSeccompDomain(t, filters, syscallNumber, unix.AF_UNIX)
}

func evaluateSeccompDomain(t *testing.T, filters []unix.SockFilter, syscallNumber, domain uint32) uint32 {
	t.Helper()
	accumulator := uint32(0)
	for pc, steps := 0, 0; pc < len(filters) && steps <= len(filters); steps++ {
		instruction := filters[pc]
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
			pc++
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if accumulator == instruction.K {
				pc += int(instruction.Jt) + 1
			} else {
				pc += int(instruction.Jf) + 1
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if accumulator&instruction.K != 0 {
				pc += int(instruction.Jt) + 1
			} else {
				pc += int(instruction.Jf) + 1
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
