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
	case SupervisedFileMode:
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
	if control.Policy.Mode == SupervisedFileMode {
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
	inputPath := filepath.Join("/work", private.InputName)
	outputPath := filepath.Join("/work", private.OutputName)
	var restarts int
	var priorOutput supervisedFileIdentity
	var hadPriorOutput bool
	for attempt := 0; ; attempt++ {
		if err := recreateSupervisedInput(inputPath, inputBytes); err != nil {
			return 0, err
		}
		if err := recreatePrivateProfile(); err != nil {
			return 0, err
		}
		var err error
		priorOutput, hadPriorOutput, err = removeSupervisedOutput(outputPath)
		if err != nil {
			return 0, err
		}
		command := exec.Command( //nolint:gosec // authenticated sealed executable and policy
			executablePath, control.Policy.Arguments...)
		command.Dir = "/work"
		command.Env = control.Policy.Environment
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		command.WaitDelay = childDrainWindow
		if err := command.Start(); err != nil {
			return 0, fmt.Errorf("start sandbox supervised child: %w", err)
		}
		runErr := command.Wait()
		if err := reapSupervisedDescendants(); err != nil {
			return 0, err
		}
		if runErr == nil {
			restarts = attempt
			break
		}
		var exitError *exec.ExitError
		if attempt == 0 && errors.As(runErr, &exitError) && exitError.ExitCode() == 81 {
			continue
		}
		return 0, fmt.Errorf("%w: sandbox supervised child failed: %w", ErrChildFailed, runErr)
	}
	if err := copySupervisedOutput(outputPath, private.MaxOutputBytes, priorOutput, hadPriorOutput); err != nil {
		return 0, err
	}
	return restarts, nil
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
	settings := []byte(privateProfileSettings())
	if err := writeRegularFileAt(int(user.Fd()), privateProfileSettingsName, settings); err != nil {
		return fmt.Errorf("write sandbox profile settings: %w", err)
	}
	if err := validateRegularFileAt(int(user.Fd()), privateProfileSettingsName, settings); err != nil {
		return fmt.Errorf("validate sandbox profile settings: %w", err)
	}
	return nil
}

func privateProfileSettings() string {
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

// PrivateProfileSettings returns the profile XML used by supervised launches.
func PrivateProfileSettings() string { return privateProfileSettings() }

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

func copySupervisedOutput(path string, maxBytes int64, prior supervisedFileIdentity, hadPrior bool) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		return errors.New("sandbox supervised child produced no output")
	}
	if info.Size() > maxBytes {
		return ErrOutputTooLarge
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("sandbox supervised output cannot be opened")
	}
	opened, statErr := file.Stat()
	if statErr != nil || opened.Mode()&os.ModeSymlink != 0 || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return errors.New("sandbox supervised output changed")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil || stat.Nlink != 1 {
		_ = file.Close()
		return errors.New("sandbox supervised output has unexpected links")
	}
	identity := supervisedFileIdentity{device: stat.Dev, inode: stat.Ino}
	if hadPrior && identity == prior {
		_ = file.Close()
		return errors.New("sandbox supervised output reused a prior file")
	}
	written, readErr := io.Copy(os.Stdout, io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.New("read sandbox supervised output failed")
	}
	if written != opened.Size() || written > maxBytes {
		return ErrOutputTooLarge
	}
	return nil
}

type supervisedFileIdentity struct {
	device uint64
	inode  uint64
}

func removeSupervisedOutput(path string) (supervisedFileIdentity, bool, error) {
	identity, err := lstatSupervisedFile(path)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return supervisedFileIdentity{}, false, nil
		}
		return supervisedFileIdentity{}, false, fmt.Errorf("stat sandbox supervised output: %w", err)
	}
	if err := removeSupervisedPath(path); err != nil {
		return supervisedFileIdentity{}, false, fmt.Errorf("remove sandbox supervised output: %w", err)
	}
	return *identity, true, nil
}

func lstatSupervisedFile(path string) (*supervisedFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return nil, fmt.Errorf("lstat sandbox supervised output: %w", err)
	}
	return &supervisedFileIdentity{device: stat.Dev, inode: stat.Ino}, nil
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
		return fmt.Errorf("create sandbox attempt file: %w", err)
	}
	return writeRegularFileFD(fd, data)
}

func writeRegularFileAt(dirfd int, name string, data []byte) error {
	fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create sandbox attempt file relative to parent: %w", err)
	}
	return writeRegularFileFD(fd, data)
}

func writeRegularFileFD(fd int, data []byte) error {
	file := os.NewFile(uintptr(fd), "sandbox-attempt-file")
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
		return fmt.Errorf("open sandbox attempt file: %w", err)
	}
	file := os.NewFile(uintptr(fd), "sandbox-attempt-file")
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("sandbox attempt file is not regular")
	}
	return validateRegularFileContents(file, stat.Size, expected)
}

func validateRegularFileAt(dirfd int, name string, expected []byte) error {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open sandbox profile settings: %w", err)
	}
	file := os.NewFile(uintptr(fd), "sandbox-attempt-file")
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("sandbox profile settings are not regular")
	}
	return validateRegularFileContents(file, stat.Size, expected)
}

func validateRegularFileContents(file *os.File, size int64, expected []byte) error {
	if size != int64(len(expected)) {
		return errors.New("sandbox attempt file has unexpected size")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(data, expected) {
		return errors.New("sandbox attempt file has unexpected contents")
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
