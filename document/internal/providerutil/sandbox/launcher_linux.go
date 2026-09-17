//go:build linux

package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func init() {
	control, authenticated := authenticatedLaunch(os.Args, launcherControlFD)
	if !authenticated {
		return
	}
	if err := runLauncher(control, launcherExecutableFD, launcherStatusFD); err != nil {
		status := launcherFailureStatus
		if statusErr, ok := errors.AsType[launcherStatusError](err); ok {
			status = statusErr.status
		}
		if errors.Is(err, ErrRuntimeIdentityMismatch) {
			status = launcherRuntimeMismatchStatus
		} else if errors.Is(err, ErrRuntimeSpecialFile) {
			status = launcherRuntimeSpecialStatus
		} else if errors.Is(err, ErrPrivateRootUnavailable) {
			status = launcherPrivateRootStatus
		}
		_, _ = fmt.Fprintln(os.Stderr, err)
		_ = writeLauncherStatus(launcherStatusFD, status)
		if errors.Is(err, ErrOutputTooLarge) {
			os.Exit(launcherOutputExitCode)
		}
		os.Exit(launcherFailureExitCode)
	}
	os.Exit(0)
}

func runLauncher(control launchControl, executableFD, statusFD int) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount propagation private: %w", err)
	}
	switch control.Policy.Mode {
	case ExecMode:
		if err := unix.MountSetattr(unix.AT_FDCWD, "/", unix.AT_RECURSIVE,
			&unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}); err != nil {
			return fmt.Errorf("make inherited host mounts read-only: %w", err)
		}
		if err := installStrictExecFilesystem(); err != nil {
			return err
		}
	case LibreOfficeMode:
		if err := installPrivateRoot(control, executableFD); err != nil {
			return err
		}
	default:
		return errors.New("sandbox mode is invalid")
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges: %w", err)
	}
	if control.Policy.Mode == ExecMode {
		if err := installExecLandlock(); err != nil {
			return err
		}
		if err := installNetworkSeccomp(execNetworkFilters); err != nil {
			return err
		}
	} else {
		if err := installPrivateLandlock(control.Policy.Executable, control.Policy.PrivateRoot); err != nil {
			return err
		}
		if err := installNetworkSeccomp(unixOnlyNetworkFilters); err != nil {
			return err
		}
	}
	if err := unix.Close(launcherControlFD); err != nil {
		return fmt.Errorf("close launcher control: %w", err)
	}
	if err := writeLauncherStatus(statusFD, launcherReadyStatus); err != nil {
		return err
	}
	unix.CloseOnExec(statusFD)
	unix.CloseOnExec(executableFD)
	if control.Policy.Mode == LibreOfficeMode {
		restarts, err := runSupervisedChild(control, control.Policy.Executable)
		if err != nil {
			status := launcherFailureStatus
			if errors.Is(err, ErrChildFailed) {
				status = launcherChildFailureStatus
			} else if errors.Is(err, ErrOutputTooLarge) {
				status = launcherOutputFailureStatus
			}
			return launcherStatusError{status: status, err: err}
		}
		if err := writeLauncherRestartStatus(statusFD, restarts); err != nil {
			return err
		}
		return nil
	}
	if err := unix.Exec("/proc/self/fd/3",
		append([]string{control.Policy.Executable}, control.Policy.Arguments...),
		control.Policy.Environment); err != nil {
		return fmt.Errorf("execute sealed bridge: %w", err)
	}
	return nil
}

func installStrictExecFilesystem() error {
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC,
		"mode=1777,size=256m"); err != nil {
		return fmt.Errorf("mount private temporary directory: %w", err)
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount private proc: %w", err)
	}
	if err := unix.Chdir("/tmp"); err != nil {
		return fmt.Errorf("enter private working directory: %w", err)
	}
	return nil
}

type launcherStatusError struct {
	status byte
	err    error
}

func (err launcherStatusError) Error() string { return err.err.Error() }

func (err launcherStatusError) Unwrap() error { return err.err }

func runSupervisedChild(control launchControl, executablePath string) (int, error) {
	private := control.Policy.PrivateRoot
	if private == nil || private.InputName == private.OutputName {
		return 0, errors.New("sandbox supervised private root is invalid")
	}
	if err := os.Mkdir("/work", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return 0, fmt.Errorf("create sandbox work directory: %w", err)
	}
	inputBytes, err := readBoundedInput(control.Policy.MaxStdinBytes)
	if err != nil {
		return 0, err
	}
	if err := recreatePrivateProfile(); err != nil {
		return 0, err
	}
	inputPath := filepath.Join("/work", private.InputName)
	outputPath := filepath.Join("/work", private.OutputName)
	if err := writeRegularFile(inputPath, private.WarmupInput); err != nil {
		return 0, fmt.Errorf("write sandbox warm-up input: %w", err)
	}
	if err := validateRegularFile(inputPath, private.WarmupInput); err != nil {
		return 0, fmt.Errorf("validate sandbox warm-up input: %w", err)
	}
	warmupEnvironment := append(slices.Clone(control.Policy.Environment), "DOCBANK_SANDBOX_WARMUP=1")
	restarts, err := runWarmup(commandSpec{
		executablePath: executablePath, arguments: control.Policy.Arguments,
		environment: warmupEnvironment, inputPath: inputPath, input: private.WarmupInput,
		outputPath:     outputPath,
		maxOutputBytes: private.MaxOutputBytes,
	})
	if err != nil {
		return 0, err
	}
	if err := cleanupWarmupState(); err != nil {
		return 0, err
	}
	if err := validatePrivateProfile(); err != nil {
		return 0, err
	}
	if err := recreateSupervisedInput(inputPath, inputBytes); err != nil {
		return 0, err
	}
	if err := ensureSupervisedPathAbsent(outputPath, "output"); err != nil {
		return 0, err
	}
	command := exec.Command( //nolint:gosec // authenticated sealed executable and policy
		executablePath, control.Policy.Arguments...)
	command.Dir = "/work"
	command.Env = control.Policy.Environment
	command.Stdout = io.Discard
	command.Stderr = os.Stderr
	command.WaitDelay = childDrainWindow
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("start sandbox supervised child: %w", err)
	}
	runErr := command.Wait()
	if err := reapSupervisedDescendants(); err != nil {
		return 0, err
	}
	if runErr != nil {
		return 0, fmt.Errorf("%w: sandbox supervised child failed: %w", ErrChildFailed, runErr)
	}
	output, err := readSupervisedOutput(outputPath, private.MaxOutputBytes)
	if err != nil {
		return 0, err
	}
	if _, err := os.Stdout.Write(output); err != nil {
		return 0, errors.New("write sandbox supervised output failed")
	}
	return restarts, nil
}

type commandSpec struct {
	executablePath string
	arguments      []string
	environment    []string
	inputPath      string
	outputPath     string
	maxOutputBytes int64
	input          []byte
}

func runWarmup(spec commandSpec) (int, error) {
	var restarts int
	for launch := range 2 {
		if err := validateRegularFile(spec.inputPath, spec.input); err != nil {
			return 0, fmt.Errorf("validate sandbox warm-up input: %w", err)
		}
		if launch > 0 {
			if err := removeSupervisedPath(spec.outputPath); err != nil {
				return 0, fmt.Errorf("remove sandbox warm-up output: %w", err)
			}
		}
		command := exec.Command( //nolint:gosec // authenticated sealed executable and policy
			spec.executablePath, spec.arguments...)
		command.Dir = "/work"
		command.Env = spec.environment
		command.Stdout = io.Discard
		command.Stderr = os.Stderr
		command.WaitDelay = childDrainWindow
		if err := command.Start(); err != nil {
			return 0, fmt.Errorf("start sandbox warm-up: %w", err)
		}
		runErr := command.Wait()
		if err := reapSupervisedDescendants(); err != nil {
			return 0, err
		}
		if runErr == nil {
			if _, err := readSupervisedOutput(spec.outputPath, spec.maxOutputBytes); err != nil {
				return 0, fmt.Errorf("validate sandbox warm-up output: %w", err)
			}
			return restarts, nil
		}
		if exitError, ok := errors.AsType[*exec.ExitError](runErr); launch == 0 && ok && exitError.ExitCode() == 81 {
			restarts = 1
			continue
		}
		return 0, fmt.Errorf("%w: sandbox warm-up failed: %w", ErrChildFailed, runErr)
	}
	return 0, errors.New("sandbox warm-up did not complete")
}

func cleanupWarmupState() error {
	entries, err := os.ReadDir("/work")
	if err != nil {
		return fmt.Errorf("read sandbox warm-up root: %w", err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "profile", "home", "out", "tmp":
			continue
		default:
			if err := os.RemoveAll(filepath.Join("/work", entry.Name())); err != nil {
				return fmt.Errorf("remove sandbox warm-up entry: %w", err)
			}
		}
	}
	for _, path := range []string{"/work/home", "/work/out"} {
		if err := clearDirectory(path); err != nil {
			return err
		}
	}
	if err := clearDirectory("/work/tmp"); err != nil {
		return err
	}
	if err := os.MkdirAll("/work/home/cache", 0o700); err != nil {
		return fmt.Errorf("recreate sandbox warm-up cache: %w", err)
	}
	return nil
}

func clearDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read sandbox warm-up directory: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
			return fmt.Errorf("clear sandbox warm-up directory: %w", err)
		}
	}
	return nil
}

func writeLauncherRestartStatus(fd, restarts int) error {
	if restarts < 0 || restarts > 255-int(launcherRestartStatusBase) {
		return errors.New("sandbox launcher restart count is outside the status range")
	}
	return writeLauncherStatus(fd, launcherRestartStatusBase+byte(restarts))
}

func reapSupervisedDescendants() error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := unix.Kill(-1, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("terminate sandbox supervised descendants: %w", err)
		}
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if errors.Is(err, unix.ECHILD) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reap sandbox supervised descendants: %w", err)
		}
		if pid > 0 {
			continue
		}
		if time.Now().After(deadline) {
			return errors.New("sandbox supervised descendants did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const privateProfileSettingsName = "registrymodifications.xcu"

func recreatePrivateProfile() error {
	profile, err := openNoFollowDirectory("/work/profile")
	if err != nil {
		return fmt.Errorf("open sandbox profile: %w", err)
	}
	defer func() { _ = profile.Close() }()
	user, err := openNoFollowDirectoryAt(int(profile.Fd()), "user")
	if err != nil {
		return fmt.Errorf("open sandbox profile user directory: %w", err)
	}
	defer func() { _ = user.Close() }()
	if err := unix.Unlinkat(int(user.Fd()), privateProfileSettingsName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove sandbox profile settings: %w", err)
	}
	settings := []byte(PrivateProfileSettings())
	if err := writeRegularFileAt(int(user.Fd()), privateProfileSettingsName, settings); err != nil {
		return fmt.Errorf("write sandbox profile settings: %w", err)
	}
	if err := validateRegularFileAt(int(user.Fd()), privateProfileSettingsName, settings); err != nil {
		return fmt.Errorf("validate sandbox profile settings: %w", err)
	}
	return nil
}

func validatePrivateProfile() error {
	profile, err := openNoFollowDirectory("/work/profile")
	if err != nil {
		return fmt.Errorf("open sandbox profile: %w", err)
	}
	defer func() { _ = profile.Close() }()
	user, err := openNoFollowDirectoryAt(int(profile.Fd()), "user")
	if err != nil {
		return fmt.Errorf("open sandbox profile user directory: %w", err)
	}
	defer func() { _ = user.Close() }()
	fd, err := unix.Openat(int(user.Fd()), privateProfileSettingsName,
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open sandbox profile settings: %w", err)
	}
	file := os.NewFile(uintptr(fd), "sandbox-profile-settings")
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("sandbox profile settings are not regular")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil || !profileSecuritySettingsPresent(data) {
		return errors.New("sandbox profile settings lost required security values")
	}
	return nil
}

func profileSecuritySettingsPresent(data []byte) bool {
	for _, value := range []string{
		`oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value>`,
		`oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value>`,
		`oor:name="DisableActiveContent" oor:op="fuse"><value>true</value>`,
		`oor:name="BlockUntrustedRefererLinks" oor:op="fuse"><value>true</value>`,
	} {
		if !bytes.Contains(data, []byte(value)) {
			return false
		}
	}
	return true
}

// PrivateProfileSettings returns the fixed LibreOffice profile XML.
func PrivateProfileSettings() string {
	return "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<oor:items xmlns:oor=\"http://openoffice.org/2001/registry\">\n" +
		" <item oor:path=\"/org.openoffice.Office.Common/Security/Scripting\">\n" +
		"  <prop oor:name=\"MacroSecurityLevel\" oor:op=\"fuse\"><value>3</value></prop>\n" +
		"  <prop oor:name=\"DisableMacrosExecution\" oor:op=\"fuse\"><value>true</value></prop>\n" +
		"  <prop oor:name=\"DisableActiveContent\" oor:op=\"fuse\"><value>true</value></prop>\n" +
		"  <prop oor:name=\"BlockUntrustedRefererLinks\" oor:op=\"fuse\"><value>true</value></prop>\n" +
		" </item>\n" +
		"</oor:items>"
}

func readBoundedInput(maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrOutputTooLarge
	}
	return data, nil
}

func ensureSupervisedPathAbsent(path, subject string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("sandbox supervised warm-up created %s", subject)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check sandbox supervised %s: %w", subject, err)
	}
	return nil
}

func recreateSupervisedInput(path string, data []byte) error {
	if err := removeSupervisedPath(path); err != nil {
		return fmt.Errorf("remove sandbox supervised input: %w", err)
	}
	if err := writeRegularFile(path, data); err != nil {
		return fmt.Errorf("write sandbox supervised input: %w", err)
	}
	if err := validateRegularFile(path, data); err != nil {
		return fmt.Errorf("validate sandbox supervised input: %w", err)
	}
	return nil
}

func readSupervisedOutput(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, errors.New("sandbox supervised child produced no output")
	}
	if info.Size() > maxBytes {
		return nil, ErrOutputTooLarge
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("sandbox supervised output cannot be opened")
	}
	opened, statErr := file.Stat()
	if statErr != nil || opened.Mode()&os.ModeSymlink != 0 || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, errors.New("sandbox supervised output changed")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil || stat.Nlink != 1 {
		_ = file.Close()
		return nil, errors.New("sandbox supervised output has unexpected links")
	}
	var output bytes.Buffer
	written, readErr := io.Copy(&output, io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.New("read sandbox supervised output failed")
	}
	if written != opened.Size() || written > maxBytes {
		return nil, ErrOutputTooLarge
	}
	return output.Bytes(), nil
}

func removeSupervisedPath(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func openNoFollowDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open sandbox profile directory: %w", err)
	}
	return verifyDirectoryFD(fd)
}

func openNoFollowDirectoryAt(dirfd int, name string) (*os.File, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open sandbox profile child directory: %w", err)
	}
	return verifyDirectoryFD(fd)
}

func verifyDirectoryFD(fd int) (*os.File, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("stat sandbox profile parent: %w", err)
		}
		return nil, errors.New("sandbox profile parent is not a directory")
	}
	return os.NewFile(uintptr(fd), "sandbox-profile-directory"), nil
}

func writeRegularFile(path string, data []byte) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create sandbox work file: %w", err)
	}
	return writeRegularFileFD(fd, data)
}

func writeRegularFileAt(dirfd int, name string, data []byte) error {
	fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create sandbox work file relative to parent: %w", err)
	}
	return writeRegularFileFD(fd, data)
}

func writeRegularFileFD(fd int, data []byte) error {
	file := os.NewFile(uintptr(fd), "sandbox-work-file")
	written, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return closeErr
}

func validateRegularFile(path string, expected []byte) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open sandbox work file: %w", err)
	}
	file := os.NewFile(uintptr(fd), "sandbox-work-file")
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("sandbox work file is not regular")
	}
	return validateRegularFileContents(file, stat.Size, expected)
}

func validateRegularFileAt(dirfd int, name string, expected []byte) error {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open sandbox profile settings: %w", err)
	}
	file := os.NewFile(uintptr(fd), "sandbox-work-file")
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("sandbox profile settings are not regular")
	}
	return validateRegularFileContents(file, stat.Size, expected)
}

func validateRegularFileContents(file *os.File, size int64, expected []byte) error {
	if size != int64(len(expected)) {
		return errors.New("sandbox work file has unexpected size")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(data, expected) {
		return errors.New("sandbox work file has unexpected contents")
	}
	return nil
}

func writeLauncherStatus(fd int, status byte) error {
	written, err := unix.Write(fd, []byte{status})
	if err != nil {
		return fmt.Errorf("write sandbox launcher status: %w", err)
	}
	if written != 1 {
		return fmt.Errorf("write sandbox launcher status: wrote %d bytes", written)
	}
	return nil
}

func installNetworkSeccomp(builder func(uint32) ([]unix.SockFilter, error)) error {
	architecture, ok := auditArchitecture()
	if !ok {
		return unix.ENOTSUP
	}
	filters, err := builder(architecture)
	if err != nil {
		return err
	}
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]} //nolint:gosec
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program))) //nolint:gosec // audited seccomp ABI pointer
	if errno != 0 {
		return errno
	}
	return nil
}
