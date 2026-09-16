//go:build linux

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const landlockBaseAccess = uint64(
	unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE)

func init() {
	control, authenticated := authenticatedLaunch(os.Args, launcherControlFD)
	if !authenticated {
		return
	}
	if err := runLauncher(control, launcherExecutableFD, launcherStatusFD); err != nil {
		_ = writeLauncherStatus(launcherStatusFD, launcherFailureStatus)
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
	executablePath := "/proc/self/fd/3"
	supervised := control.Policy.supervision().Mode == SupervisedFileMode
	if supervised {
		workBytes := control.Policy.supervision().WorkBytes
		if workBytes <= 0 {
			workBytes = 256 << 20
		}
		if err := unix.Mount("tmpfs", "/tmp", "tmpfs",
			unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, fmt.Sprintf("mode=1777,size=%d", workBytes)); err != nil {
			return fmt.Errorf("mount private temporary directory: %w", err)
		}
		var err error
		executablePath, err = prepareSupervisedExecutable(control, executableFD)
		if err != nil {
			return err
		}
	}
	if err := unix.MountSetattr(unix.AT_FDCWD, "/", unix.AT_RECURSIVE,
		&unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}); err != nil {
		return fmt.Errorf("make inherited host mounts read-only: %w", err)
	}
	if supervised {
		workBytes := control.Policy.supervision().WorkBytes
		if workBytes <= 0 {
			workBytes = 256 << 20
		}
		if err := unix.Mount("", "/tmp", "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC,
			fmt.Sprintf("mode=1777,size=%d", workBytes)); err != nil {
			return fmt.Errorf("make private temporary directory writable: %w", err)
		}
	}
	if !supervised {
		workBytes := control.Policy.supervision().WorkBytes
		if workBytes <= 0 {
			workBytes = 256 << 20
		}
		if err := unix.Mount("tmpfs", "/tmp", "tmpfs",
			unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, fmt.Sprintf("mode=1777,size=%d", workBytes)); err != nil {
			return fmt.Errorf("mount private temporary directory: %w", err)
		}
	}
	if err := unix.Mount("proc", "/proc", "proc",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount private proc: %w", err)
	}
	if err := unix.Chdir("/tmp"); err != nil {
		return fmt.Errorf("enter private working directory: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges: %w", err)
	}
	if err := installFilesystemLandlock(control.Policy.ReadOnlyPaths); err != nil {
		return err
	}
	if err := installNetworkSeccomp(); err != nil {
		return err
	}
	if err := unix.Close(launcherControlFD); err != nil {
		return fmt.Errorf("close launcher control: %w", err)
	}
	if err := writeLauncherStatus(statusFD, launcherReadyStatus); err != nil {
		return err
	}
	unix.CloseOnExec(statusFD)
	if control.Policy.supervision().Mode == SupervisedFileMode {
		if err := runSupervisedChild(control, executablePath); err != nil {
			return err
		}
		return nil
	}
	if err := unix.Exec("/proc/self/fd/3", append([]string{control.Policy.Executable}, control.Policy.Arguments...), control.Policy.Environment); err != nil {
		return fmt.Errorf("execute sealed bridge: %w", err)
	}
	return nil
}

func prepareSupervisedExecutable(control launchControl, executableFD int) (string, error) {
	if err := os.MkdirAll("/tmp/work", 0o700); err != nil {
		return "", fmt.Errorf("create sandbox executable directory: %w", err)
	}
	if strings.HasPrefix(filepath.Clean(control.Policy.Executable), "/tmp/") {
		return "/proc/self/fd/3", nil
	}
	sourceFD, err := unix.Dup(executableFD)
	if err != nil {
		return "", fmt.Errorf("duplicate sandbox executable: %w", err)
	}
	source := os.NewFile(uintptr(sourceFD), "sandbox-sealed-executable")
	defer func() { _ = source.Close() }()
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek sandbox executable: %w", err)
	}
	content, err := io.ReadAll(io.LimitReader(source, MaxExecutableBytes+1))
	if err != nil || int64(len(content)) <= 0 || int64(len(content)) > MaxExecutableBytes || digestContent(content) != control.Policy.ExecutableSHA256 {
		return "", errors.New("sandbox executable copy changed")
	}
	copyPath := "/tmp/work/runner"
	copyFile, err := os.OpenFile(copyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create sandbox executable copy: %w", err)
	}
	written, writeErr := copyFile.Write(content)
	closeErr := copyFile.Close()
	if writeErr != nil || closeErr != nil || written != len(content) {
		return "", errors.New("write sandbox executable copy failed")
	}
	if err := os.Chmod(copyPath, 0o500); err != nil { //nolint:gosec // the private executable copy must be owner-executable
		return "", errors.New("make sandbox executable copy runnable")
	}
	if err := unix.Mount(copyPath, control.Policy.Executable, "", unix.MS_BIND, ""); err != nil {
		return "", fmt.Errorf("bind sealed sandbox executable copy: %w", err)
	}
	if err := unix.Mount("", control.Policy.Executable, "", unix.MS_REMOUNT|unix.MS_BIND, ""); err != nil {
		return "", fmt.Errorf("make sealed sandbox executable executable: %w", err)
	}
	return control.Policy.Executable, nil
}

func digestContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func runSupervisedChild(control launchControl, executablePath string) error {
	supervision := control.Policy.supervision()
	if supervision.InputName == supervision.OutputName {
		return errors.New("sandbox supervised input and output names must differ")
	}
	if err := os.Mkdir("/tmp/work", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create sandbox work directory: %w", err)
	}
	for _, directory := range []string{"profile", "home", "tmp"} {
		if err := os.Mkdir(filepath.Join("/tmp/work", directory), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create sandbox work subdirectory: %w", err)
		}
	}
	if err := writePrivateProfile(); err != nil {
		return err
	}
	inputPath := filepath.Join("/tmp/work", supervision.InputName)
	outputPath := filepath.Join("/tmp/work", supervision.OutputName)
	input, err := os.OpenFile(inputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create sandbox supervised input: %w", err)
	}
	readErr := writeBoundedInput(input, control.Policy.MaxStdinBytes)
	closeErr := input.Close()
	if readErr != nil || closeErr != nil {
		return errors.New("write sandbox supervised input failed")
	}
	for attempt := 0; ; attempt++ {
		command := exec.Command( //nolint:gosec // the sealed executable and complete policy are authenticated before this point
			executablePath, append([]string{control.Policy.Executable}, control.Policy.Arguments...)...,
		)
		command.Dir = "/tmp/work"
		command.Env = control.Policy.Environment
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		command.WaitDelay = childDrainWindow
		if err := command.Start(); err != nil {
			return fmt.Errorf("start sandbox supervised child: %w", err)
		}
		runErr := command.Wait()
		if runErr == nil {
			break
		}
		var exitError *exec.ExitError
		if attempt == 0 && errors.As(runErr, &exitError) && exitError.ExitCode() == 81 {
			continue
		}
		return fmt.Errorf("sandbox supervised child failed: %w", runErr)
	}
	return copySupervisedOutput(outputPath, supervision.MaxOutputBytes)
}

func writePrivateProfile() error {
	if err := os.MkdirAll("/tmp/work/profile/user", 0o700); err != nil {
		return fmt.Errorf("create sandbox profile: %w", err)
	}
	const settings = `<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry">
 <item oor:path="/org.openoffice.Office.Common/Security">
  <prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop>
  <prop oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value></prop>
  <prop oor:name="DisableActiveContent" oor:op="fuse"><value>true</value></prop>
 </item>
 <item oor:path="/org.openoffice.Office.Common/Load">
  <prop oor:name="UpdateDocMode" oor:op="fuse"><value>0</value></prop>
 </item>
</oor:items>`
	if err := os.WriteFile("/tmp/work/profile/user/registrymodifications.xcu", []byte(settings), 0o600); err != nil { //nolint:gosec // the path is inside the private sandbox work mount
		return fmt.Errorf("write sandbox profile: %w", err)
	}
	return nil
}

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

func installFilesystemLandlock(extra []string) error {
	version, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION, 0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("query Landlock filesystem ABI: %w", errno)
	}
	if version < minimumLandlockABI {
		return fmt.Errorf("require Landlock filesystem ABI %d, host provides %d", minimumLandlockABI, version)
	}
	handledAccess := landlockBaseAccess
	if version >= 5 {
		handledAccess |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	rulesetAttribute := unix.LandlockRulesetAttr{Access_fs: handledAccess}
	rulesetFD, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		//nolint:gosec // Landlock requires a pointer to this fixed kernel ABI structure.
		uintptr(unsafe.Pointer(&rulesetAttribute)), unsafe.Sizeof(rulesetAttribute), 0, 0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("create Landlock filesystem ruleset: %w", errno)
	}
	defer func() { _ = unix.Close(int(rulesetFD)) }()
	readOnly := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	paths := []string{"/usr", "/lib", "/lib64", "/bin", "/proc", "/etc"}
	paths = append(paths, extra...)
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, found := seen[path]; found {
			continue
		}
		seen[path] = struct{}{}
		if err := addLandlockPath(rulesetFD, path, readOnly, true); err != nil {
			return err
		}
	}
	for _, path := range []string{"/etc/ld.so.cache", "/etc/localtime"} {
		if err := addLandlockPath(rulesetFD, path, unix.LANDLOCK_ACCESS_FS_READ_FILE, true); err != nil {
			return err
		}
	}
	for path, access := range map[string]uint64{
		"/dev/null":    unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE,
		"/dev/urandom": unix.LANDLOCK_ACCESS_FS_READ_FILE,
		"/dev/zero":    unix.LANDLOCK_ACCESS_FS_READ_FILE,
	} {
		if err := addLandlockPath(rulesetFD, path, access, true); err != nil {
			return err
		}
	}
	privateTemporaryAccess := handledAccess &^ (unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)
	if err := addLandlockPath(rulesetFD, "/tmp", privateTemporaryAccess, false); err != nil {
		return err
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, rulesetFD, 0, 0); errno != 0 {
		return fmt.Errorf("enforce Landlock filesystem ruleset: %w", errno)
	}
	return nil
}

func addLandlockPath(rulesetFD uintptr, path string, allowedAccess uint64, optional bool) error {
	pathFD, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if optional && errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open Landlock path %q: %w", path, err)
	}
	defer func() { _ = unix.Close(pathFD) }()
	pathAttribute := unix.LandlockPathBeneathAttr{
		Allowed_access: allowedAccess,
		Parent_fd:      int32(pathFD), //nolint:gosec // Linux file descriptors fit the kernel's signed 32-bit ABI field.
	}
	if _, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE, rulesetFD, unix.LANDLOCK_RULE_PATH_BENEATH,
		//nolint:gosec // Landlock requires a pointer to this fixed kernel ABI structure.
		uintptr(unsafe.Pointer(&pathAttribute)), 0, 0, 0,
	); errno != 0 {
		return fmt.Errorf("add Landlock path %q: %w", path, errno)
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

func installNetworkSeccomp() error {
	architecture, ok := auditArchitecture()
	if !ok {
		return unix.ENOTSUP
	}
	filters, err := buildNetworkSeccompFilters(architecture)
	if err != nil {
		return err
	}
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]} //nolint:gosec // filter count is fixed and bounded below uint16
	//nolint:gosec // SockFprog requires the audited kernel ABI pointer for the bounded filter slice.
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	if errno != 0 {
		return errno
	}
	return nil
}

func buildNetworkSeccompFilters(architecture uint32) ([]unix.SockFilter, error) {
	if architecture != unix.AUDIT_ARCH_X86_64 && architecture != unix.AUDIT_ARCH_AARCH64 {
		return nil, unix.ENOTSUP
	}
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: architecture},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	if architecture == unix.AUDIT_ARCH_X86_64 {
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: nativeX32SyscallBit},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	filters = append(filters,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 4, K: uint32(unix.SYS_SOCKET)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: uint32(unix.AF_UNIX)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 4, K: uint32(unix.SYS_SOCKETPAIR)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: uint32(unix.AF_UNIX)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	)
	for _, syscallNumber := range blockedNetworkSyscalls() {
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(syscallNumber)}, //nolint:gosec // Linux syscall numbers are nonnegative uint32 values
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	filters = append(filters, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	return filters, nil
}

func auditArchitecture() (uint32, bool) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, true
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, true
	default:
		return 0, false
	}
}

func blockedNetworkSyscalls() []uintptr {
	return []uintptr{
		unix.SYS_SENDTO, unix.SYS_SENDMSG, unix.SYS_SENDMMSG,
		unix.SYS_RECVFROM, unix.SYS_RECVMSG, unix.SYS_RECVMMSG,
		unix.SYS_SHUTDOWN, unix.SYS_GETSOCKNAME, unix.SYS_GETPEERNAME,
		unix.SYS_SETSOCKOPT, unix.SYS_GETSOCKOPT,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	}
}
