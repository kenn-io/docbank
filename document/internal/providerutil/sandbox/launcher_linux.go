//go:build linux

package sandbox

import (
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
	if err := writePrivateProfile(); err != nil {
		return 0, err
	}
	inputPath := filepath.Join("/work", private.InputName)
	outputPath := filepath.Join("/work", private.OutputName)
	input, err := os.OpenFile(inputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create sandbox supervised input: %w", err)
	}
	readErr := writeBoundedInput(input, control.Policy.MaxStdinBytes)
	closeErr := input.Close()
	if readErr != nil || closeErr != nil {
		return 0, errors.New("write sandbox supervised input failed")
	}
	var restarts int
	for attempt := 0; ; attempt++ {
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
	if err := copySupervisedOutput(outputPath, private.MaxOutputBytes); err != nil {
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

func writePrivateProfile() error {
	settings := privateProfileSettings()
	if err := os.MkdirAll("/work/profile/user", 0o700); err != nil {
		return fmt.Errorf("create sandbox profile: %w", err)
	}
	if err := os.WriteFile("/work/profile/user/registrymodifications.xcu", []byte(settings), 0o600); err != nil {
		return fmt.Errorf("write sandbox profile: %w", err)
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

func writeBoundedInput(file *os.File, maxBytes int64) error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxBytes {
		return ErrOutputTooLarge
	}
	written, err := file.Write(data)
	if err != nil || written != len(data) {
		return err
	}
	return nil
}

func copySupervisedOutput(path string, maxBytes int64) error {
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
