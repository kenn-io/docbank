//go:build linux

package trafilatura

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
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	nativeRunnerIdentity   = "sha256:4fb9848ec9197e12848559df3002bd39ceaa0377ae994bc8fbe9c8f082288d9e"
	nativeChildDrainWindow = 250 * time.Millisecond
)

type nativeRunner struct{}

func newNativeRunner() (IsolatedRunner, error) {
	return nativeRunner{}, nil
}

func (nativeRunner) Identity() string {
	return nativeRunnerIdentity
}

func (runner nativeRunner) Run(
	ctx context.Context, request IsolatedRunRequest,
) (IsolatedRunResult, error) {
	if err := validateNativeRequest(request); err != nil {
		return IsolatedRunResult{}, ErrIsolationUnavailable
	}
	if err := ctx.Err(); err != nil {
		return IsolatedRunResult{}, errors.Join(errNativeCanceledBeforeLaunch, err)
	}
	executable, err := openVerifiedExecutable(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return IsolatedRunResult{}, errors.Join(errNativeCanceledBeforeLaunch, ctx.Err())
		}
		return IsolatedRunResult{}, ErrIsolationUnavailable
	}
	defer func() { _ = executable.Close() }()
	control, launchToken, err := openNativeLaunchControl()
	if err != nil {
		return IsolatedRunResult{}, ErrIsolationUnavailable
	}
	defer func() { _ = control.Close() }()
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		return IsolatedRunResult{}, ErrIsolationUnavailable
	}
	defer func() { _ = statusReader.Close() }()
	defer func() { _ = statusWriter.Close() }()
	if err := ctx.Err(); err != nil {
		return IsolatedRunResult{}, errors.Join(errNativeCanceledBeforeLaunch, err)
	}

	output := &nativeBoundedOutput{limit: request.MaxStdoutBytes}
	command := exec.Command( //nolint:gosec // request validation fixes the executable fd and complete argument vector
		"/proc/self/exe", nativeLauncherMarker, launchToken, request.Executable,
	)
	command.Dir = request.Directory
	command.Env = slices.Clone(request.Environment)
	command.Stdin = bytes.NewReader(request.Stdin)
	command.Stdout = output
	command.Stderr = io.Discard
	command.ExtraFiles = []*os.File{executable, control, statusWriter}
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET |
			syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
	command.WaitDelay = nativeChildDrainWindow
	output.onOverflow = func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}
	if err := command.Start(); err != nil {
		return IsolatedRunResult{}, ErrIsolationUnavailable
	}
	_ = statusWriter.Close()

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	launcherStatus := make(chan bool, 1)
	go func() { launcherStatus <- nativeLauncherFailed(statusReader) }()
	var runErr error
	select {
	case runErr = <-waited:
	case <-ctx.Done():
		_ = command.Process.Kill()
		runErr = <-waited
	}
	launcherFailed := <-launcherStatus
	result := IsolatedRunResult{
		Stdout: output.Bytes(), Attestation: nativeAttestation(request),
	}
	if output.Exceeded() {
		return result, ErrChildOutputTooLarge
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := classifyNativeRunError(runErr, launcherFailed); err != nil {
		return result, err
	}
	return result, nil
}

func nativeLauncherFailed(reader io.Reader) bool {
	status, err := io.ReadAll(io.LimitReader(reader, 3))
	return err != nil || !bytes.Equal(status, []byte{nativeLauncherReadyStatus})
}

func classifyNativeRunError(runErr error, launcherFailed bool) error {
	if launcherFailed {
		return ErrIsolationUnavailable
	}
	if runErr != nil {
		return ErrChildFailed
	}
	return nil
}

func openNativeLaunchControl() (*os.File, string, error) {
	token := make([]byte, nativeLauncherTokenBytes)
	if _, err := rand.Read(token); err != nil {
		return nil, "", fmt.Errorf("create launcher token: %w", err)
	}
	fd, err := unix.MemfdCreate("docbank-trafilatura-launch", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, "", fmt.Errorf("create launcher control: %w", err)
	}
	file := os.NewFile(uintptr(fd), "docbank-trafilatura-launch")
	valid := false
	defer func() {
		if !valid {
			_ = file.Close()
		}
	}()
	if _, err := file.Write(token); err != nil {
		return nil, "", err
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return nil, "", fmt.Errorf("seal launcher control: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	valid = true
	return file, hex.EncodeToString(token), nil
}

func validateNativeRequest(request IsolatedRunRequest) error {
	stdinDigest := sha256.Sum256(request.Stdin)
	if !filepath.IsAbs(request.Executable) || filepath.Clean(request.Executable) != request.Executable ||
		request.Directory != filepath.Dir(request.Executable) ||
		!slices.Equal(request.Arguments, []string{"--protocol", protocolVersion}) ||
		!slices.Equal(request.Environment, cleanEnvironment()) ||
		request.StdinSHA256 != hex.EncodeToString(stdinDigest[:]) ||
		request.MaxStdoutBytes <= 0 || request.MaxStdoutBytes > MaxResponseBytes ||
		!request.Requirements.NetworkDisabled || !request.Requirements.KillProcessTree ||
		!request.Requirements.VerifyExecutableSHA256 ||
		request.PolicyFingerprint != isolationRequestPolicyFingerprint(nativeRunnerIdentity, request) {
		return errors.New("native isolation request is outside the fixed policy")
	}
	if err := validateSHA256(request.ExecutableSHA256, "executable SHA-256"); err != nil {
		return err
	}
	return nil
}

func openVerifiedExecutable(ctx context.Context, request IsolatedRunRequest) (*os.File, error) {
	source, err := os.Open(request.Executable)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxExecutableBytes {
		return nil, errors.New("executable identity is outside the supported bound")
	}
	fd, err := unix.MemfdCreate("docbank-trafilatura", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("create sealed executable: %w", err)
	}
	file := os.NewFile(uintptr(fd), "docbank-trafilatura")
	valid := false
	defer func() {
		if !valid {
			_ = file.Close()
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(
		io.MultiWriter(file, hash),
		io.LimitReader(contextReader{ctx: ctx, reader: source}, MaxExecutableBytes+1),
	)
	if err != nil || written != info.Size() || hex.EncodeToString(hash.Sum(nil)) != request.ExecutableSHA256 {
		return nil, errors.New("executable identity changed")
	}
	if err := unix.Fchmod(fd, 0o500); err != nil {
		return nil, fmt.Errorf("make sealed executable runnable: %w", err)
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return nil, fmt.Errorf("seal executable content: %w", err)
	}
	applied, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || applied&seals != seals {
		return nil, errors.New("executable content could not be sealed")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	valid = true
	return file, nil
}

func nativeAttestation(request IsolatedRunRequest) IsolationAttestation {
	return IsolationAttestation{
		RunnerIdentity: nativeRunnerIdentity, PolicyFingerprint: request.PolicyFingerprint,
		ExecutableSHA256: request.ExecutableSHA256, StdinSHA256: request.StdinSHA256,
		NetworkDisabled: true, ProcessTreeContained: true, DigestVerifiedLaunch: true,
	}
}

type nativeBoundedOutput struct {
	mu         sync.Mutex
	data       []byte
	limit      int64
	exceeded   bool
	overflow   sync.Once
	onOverflow func()
}

func (output *nativeBoundedOutput) Write(value []byte) (int, error) {
	output.mu.Lock()
	remaining := output.limit - int64(len(output.data))
	if remaining > 0 {
		kept := min(int64(len(value)), remaining)
		output.data = append(output.data, value[:kept]...)
	}
	exceeded := int64(len(value)) > remaining
	if exceeded {
		output.exceeded = true
	}
	output.mu.Unlock()
	if exceeded {
		output.overflow.Do(output.onOverflow)
	}
	return len(value), nil
}

func (output *nativeBoundedOutput) Bytes() []byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	return slices.Clone(output.data)
}

func (output *nativeBoundedOutput) Exceeded() bool {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.exceeded
}

var _ IsolatedRunner = nativeRunner{}
