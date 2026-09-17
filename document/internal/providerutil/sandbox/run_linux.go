//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"go.kenn.io/docbank/document/internal/providerutil"
)

const (
	NativeRunnerIdentity        = "sha256:afc3202c30a20fbb62fe6b7a4ccf282dea0ba16e40d22e7bdfa3f1d28819db16"
	launcherMarker              = "--docbank-internal-sandbox-launch-v1"
	launcherExecutableFD        = 3
	launcherControlFD           = 4
	launcherStatusFD            = 5
	launcherTokenBytes          = 32
	launcherFailureExitCode     = 125
	launcherOutputExitCode      = 124
	launcherReadyStatus         = byte(1)
	launcherFailureStatus       = byte(2)
	launcherChildFailureStatus  = byte(3)
	launcherOutputFailureStatus = byte(4)
	launcherRestartStatusBase   = byte(16)
	childDrainWindow            = 250 * time.Millisecond
)

// Runner is the platform process boundary used by document providers.
type Runner interface {
	Identity() string
	Run(ctx context.Context, request Request) (Result, error)
}

type nativeRunner struct{}

// NewNativeRunner returns the enforcing runner for the current platform.
func NewNativeRunner() (Runner, error) {
	return nativeRunner{}, nil
}

// Run executes one request with the native runner.
func Run(ctx context.Context, request Request) (Result, error) {
	runner, err := NewNativeRunner()
	if err != nil {
		return Result{}, err
	}
	return runner.Run(ctx, request)
}

func (nativeRunner) Identity() string { return NativeRunnerIdentity }

func (nativeRunner) Run(ctx context.Context, request Request) (Result, error) {
	if err := request.validate(); err != nil {
		return Result{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Result{}, errors.Join(ErrCanceledBeforeLaunch, err)
	}
	executable, err := openVerifiedExecutable(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, errors.Join(ErrCanceledBeforeLaunch, ctx.Err())
		}
		return Result{}, ErrUnavailable
	}
	defer func() { _ = executable.Close() }()
	var runtimeFiles []*os.File
	if request.Policy.Mode == SupervisedFileMode {
		runtimeFiles, err = prepareRuntimeFiles(ctx, request.Policy.PrivateRoot)
		if err != nil {
			return Result{}, ErrUnavailable
		}
		defer func() {
			for _, file := range runtimeFiles {
				_ = file.Close()
			}
		}()
	}
	control, launchToken, err := openLaunchControl(request)
	if err != nil {
		return Result{}, ErrUnavailable
	}
	defer func() { _ = control.Close() }()
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		return Result{}, ErrUnavailable
	}
	defer func() { _ = statusReader.Close() }()
	defer func() { _ = statusWriter.Close() }()
	if err := ctx.Err(); err != nil {
		return Result{}, errors.Join(ErrCanceledBeforeLaunch, err)
	}

	var command *exec.Cmd
	output := providerutil.NewBoundedBuffer(request.Policy.MaxStdoutBytes, func() {
		if command != nil && command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	command = exec.Command( //nolint:gosec // the sealed executable and complete policy are authenticated below
		"/proc/self/exe", launcherMarker, launchToken, request.Policy.Executable,
	)
	command.Dir = request.Policy.Directory
	command.Env = slices.Clone(request.Policy.Environment)
	command.Stdin = bytes.NewReader(request.Stdin)
	command.Stdout = output
	command.Stderr = io.Discard
	command.ExtraFiles = append([]*os.File{executable, control, statusWriter}, runtimeFiles...)
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET |
			syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
	command.WaitDelay = childDrainWindow
	if err := command.Start(); err != nil {
		return Result{}, ErrUnavailable
	}
	_ = executable.Close()
	for _, file := range runtimeFiles {
		_ = file.Close()
	}
	_ = statusWriter.Close()

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	launcherStatus := make(chan launcherRunStatus, 1)
	go func() { launcherStatus <- launcherStatusRead(statusReader) }()
	var runErr error
	select {
	case runErr = <-waited:
	case <-ctx.Done():
		_ = command.Process.Kill()
		runErr = <-waited
	}
	status := <-launcherStatus
	result := Result{
		Stdout: output.Bytes(),
		Attestation: Attestation{
			RunnerIdentity: NativeRunnerIdentity, PolicyFingerprint: request.PolicyFingerprint,
			ExecutableSHA256: request.Policy.ExecutableSHA256, StdinSHA256: request.StdinSHA256,
			NetworkDisabled: true, ProcessTreeContained: true, DigestVerifiedLaunch: true,
			FilesystemIsolated: true, FilesystemMode: filesystemMode(request.Policy.Mode),
			PrivateRootInstalled: request.Policy.Mode == SupervisedFileMode,
			RuntimeIdentity:      runtimeIdentity(request.Policy),
			UnixIPCAllowed:       request.Policy.Mode == SupervisedFileMode,
			RestartCount:         status.restarts,
		},
	}
	result.Output = slices.Clone(result.Stdout)
	if output.Exceeded() {
		return result, ErrOutputTooLarge
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !status.ready {
		return result, ErrUnavailable
	}
	if status.detail == launcherFailureStatus {
		return result, ErrUnavailable
	}
	if status.detail == launcherOutputFailureStatus {
		return result, ErrOutputTooLarge
	}
	if status.detail == launcherChildFailureStatus {
		return result, ErrChildFailed
	}
	if runErr != nil {
		return result, ErrChildFailed
	}
	return result, nil
}

type launcherRunStatus struct {
	ready    bool
	detail   byte
	restarts int
}

func launcherStatusRead(reader io.Reader) launcherRunStatus {
	data, err := io.ReadAll(io.LimitReader(reader, 3))
	if err != nil || len(data) == 0 || data[0] != launcherReadyStatus {
		return launcherRunStatus{}
	}
	status := launcherRunStatus{ready: true}
	if len(data) < 2 {
		return status
	}
	switch data[1] {
	case launcherRestartStatusBase:
		status.restarts = 0
	case launcherFailureStatus, launcherChildFailureStatus, launcherOutputFailureStatus:
		status.detail = data[1]
	default:
		if data[1] >= launcherRestartStatusBase {
			status.restarts = int(data[1] - launcherRestartStatusBase)
		} else {
			status.detail = launcherFailureStatus
		}
	}
	return status
}

func launcherReadyStatusRead(reader io.Reader) bool {
	return launcherStatusRead(reader).ready
}

func runtimeIdentity(policy Policy) string {
	if policy.PrivateRoot == nil {
		return ""
	}
	return policy.PrivateRoot.RuntimeIdentity
}

func filesystemMode(mode Mode) string {
	if mode == SupervisedFileMode {
		return "private-root-v1"
	}
	return "strict-exec"
}

func openLaunchControl(request Request) (*os.File, string, error) {
	encoded, err := encodeControl(launchControl{
		Policy: request.Policy, PolicyFingerprint: request.PolicyFingerprint,
		StdinSHA256: request.StdinSHA256,
	})
	if err != nil {
		return nil, "", err
	}
	token := make([]byte, launcherTokenBytes)
	if _, err := rand.Read(token); err != nil {
		return nil, "", fmt.Errorf("create sandbox launcher token: %w", err)
	}
	fd, err := unix.MemfdCreate("docbank-sandbox-launch", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, "", fmt.Errorf("create sandbox launcher control: %w", err)
	}
	file := os.NewFile(uintptr(fd), "docbank-sandbox-launch")
	valid := false
	defer func() {
		if !valid {
			_ = file.Close()
		}
	}()
	controlBytes := make([]byte, 0, len(token)+len(encoded))
	controlBytes = append(controlBytes, token...)
	controlBytes = append(controlBytes, encoded...)
	if _, err := file.Write(controlBytes); err != nil {
		return nil, "", err
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return nil, "", fmt.Errorf("seal sandbox launcher control: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	valid = true
	return file, hex.EncodeToString(token), nil
}

func authenticatedLaunch(arguments []string, controlFD int) (launchControl, bool) {
	if len(arguments) != 4 || arguments[1] != launcherMarker ||
		!filepath.IsAbs(arguments[3]) || filepath.Clean(arguments[3]) != arguments[3] {
		return launchControl{}, false
	}
	want, err := hex.DecodeString(arguments[2])
	if err != nil || len(want) != launcherTokenBytes {
		return launchControl{}, false
	}
	var stat unix.Stat_t
	if err := unix.Fstat(controlFD, &stat); err != nil ||
		stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Size < launcherTokenBytes || stat.Size > launcherTokenBytes+MaxPrivateRootControlBytes {
		return launchControl{}, false
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	applied, err := unix.FcntlInt(uintptr(controlFD), unix.F_GET_SEALS, 0)
	if err != nil || applied&seals != seals {
		return launchControl{}, false
	}
	content := make([]byte, stat.Size)
	read, err := unix.Pread(controlFD, content, 0)
	if err != nil || read != len(content) || !bytes.Equal(content[:launcherTokenBytes], want) {
		return launchControl{}, false
	}
	control, err := decodeControl(content[launcherTokenBytes:])
	if err != nil {
		return launchControl{}, false
	}
	if control.Policy.Executable != arguments[3] {
		return launchControl{}, false
	}
	return control, true
}

func openVerifiedExecutable(ctx context.Context, request Request) (*os.File, error) {
	source, err := os.Open(request.Policy.Executable)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxExecutableBytes {
		return nil, errors.New("sandbox executable identity is outside the supported bound")
	}
	fd, err := unix.MemfdCreate("docbank-sandbox-executable", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("create sealed sandbox executable: %w", err)
	}
	file := os.NewFile(uintptr(fd), "docbank-sandbox-executable")
	valid := false
	defer func() {
		if !valid {
			_ = file.Close()
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, source}, MaxExecutableBytes+1))
	if err != nil || written != info.Size() || hex.EncodeToString(hash.Sum(nil)) != request.Policy.ExecutableSHA256 {
		return nil, errors.New("sandbox executable identity changed")
	}
	if err := unix.Fchmod(fd, 0o500); err != nil {
		return nil, fmt.Errorf("make sandbox executable runnable: %w", err)
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return nil, fmt.Errorf("seal sandbox executable content: %w", err)
	}
	applied, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || applied&seals != seals {
		return nil, errors.New("sandbox executable content could not be sealed")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	valid = true
	return file, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(value []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := reader.reader.Read(value)
	if contextErr := reader.ctx.Err(); contextErr != nil {
		return n, contextErr
	}
	return n, err
}
