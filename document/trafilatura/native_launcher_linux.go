//go:build linux

package trafilatura

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	nativeLauncherMarker          = "--docbank-internal-trafilatura-launch-v3"
	nativeLauncherExecutableFD    = 3
	nativeLauncherControlFD       = 4
	nativeLauncherStatusFD        = 5
	nativeLauncherTokenBytes      = 32
	nativeLauncherFailureExitCode = 125
	nativeLauncherReadyStatus     = byte(1)
	nativeLauncherFailureStatus   = byte(2)
	nativeX32SyscallBit           = uint32(0x40000000)
	nativeMinimumLandlockABI      = uintptr(3)
)

const nativeLandlockBaseAccess = uint64(
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
	executable, authenticated := authenticatedNativeLaunch(os.Args, nativeLauncherControlFD)
	if !authenticated {
		return
	}
	if runNativeLauncher(executable, nativeLauncherStatusFD) != nil {
		_ = writeNativeLauncherStatus(nativeLauncherStatusFD, nativeLauncherFailureStatus)
		os.Exit(nativeLauncherFailureExitCode)
	}
	_ = writeNativeLauncherStatus(nativeLauncherStatusFD, nativeLauncherFailureStatus)
	os.Exit(nativeLauncherFailureExitCode)
}

func authenticatedNativeLaunch(arguments []string, controlFD int) (string, bool) {
	if len(arguments) != 4 || arguments[1] != nativeLauncherMarker ||
		!filepath.IsAbs(arguments[3]) || filepath.Clean(arguments[3]) != arguments[3] {
		return "", false
	}
	want, err := hex.DecodeString(arguments[2])
	if err != nil || len(want) != nativeLauncherTokenBytes {
		return "", false
	}
	var stat unix.Stat_t
	if err := unix.Fstat(controlFD, &stat); err != nil ||
		stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size != nativeLauncherTokenBytes {
		return "", false
	}
	seals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	applied, err := unix.FcntlInt(uintptr(controlFD), unix.F_GET_SEALS, 0)
	if err != nil || applied&seals != seals {
		return "", false
	}
	got := make([]byte, nativeLauncherTokenBytes)
	read, err := unix.Pread(controlFD, got, 0)
	if err != nil || read != len(got) || subtle.ConstantTimeCompare(got, want) != 1 {
		return "", false
	}
	return arguments[3], true
}

func runNativeLauncher(executable string, statusFD int) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount propagation private: %w", err)
	}
	if err := unix.MountSetattr(unix.AT_FDCWD, "/", unix.AT_RECURSIVE,
		&unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}); err != nil {
		return fmt.Errorf("make inherited host mounts read-only: %w", err)
	}
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=1777,size=256m"); err != nil {
		return fmt.Errorf("mount private temporary directory: %w", err)
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
	if err := installNativeFilesystemLandlock(); err != nil {
		return err
	}
	if err := installNativeNetworkSeccomp(); err != nil {
		return err
	}
	if err := unix.Close(nativeLauncherControlFD); err != nil {
		return fmt.Errorf("close launcher control: %w", err)
	}
	unix.CloseOnExec(nativeLauncherExecutableFD)
	if err := writeNativeLauncherStatus(statusFD, nativeLauncherReadyStatus); err != nil {
		return err
	}
	unix.CloseOnExec(statusFD)
	if err := unix.Exec("/proc/self/fd/3", []string{executable, "--protocol", protocolVersion}, cleanEnvironment()); err != nil {
		return fmt.Errorf("execute sealed bridge: %w", err)
	}
	return nil
}

func installNativeFilesystemLandlock() error {
	version, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION, 0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("query Landlock filesystem ABI: %w", errno)
	}
	if version < nativeMinimumLandlockABI {
		return fmt.Errorf("require Landlock filesystem ABI %d, host provides %d",
			nativeMinimumLandlockABI, version)
	}
	handledAccess := nativeLandlockBaseAccess
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
	for _, path := range []string{"/usr", "/lib", "/lib64", "/bin", "/proc"} {
		if err := addNativeLandlockPath(rulesetFD, path, readOnly, true); err != nil {
			return err
		}
	}
	for _, path := range []string{"/etc/ld.so.cache", "/etc/localtime"} {
		if err := addNativeLandlockPath(rulesetFD, path, unix.LANDLOCK_ACCESS_FS_READ_FILE, true); err != nil {
			return err
		}
	}
	for path, access := range map[string]uint64{
		"/dev/null":    unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE,
		"/dev/urandom": unix.LANDLOCK_ACCESS_FS_READ_FILE,
		"/dev/zero":    unix.LANDLOCK_ACCESS_FS_READ_FILE,
	} {
		if err := addNativeLandlockPath(rulesetFD, path, access, true); err != nil {
			return err
		}
	}
	privateTemporaryAccess := handledAccess &^ (unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)
	if err := addNativeLandlockPath(rulesetFD, "/tmp", privateTemporaryAccess, false); err != nil {
		return err
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, rulesetFD, 0, 0); errno != 0 {
		return fmt.Errorf("enforce Landlock filesystem ruleset: %w", errno)
	}
	return nil
}

func addNativeLandlockPath(rulesetFD uintptr, path string, allowedAccess uint64, optional bool) error {
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

func writeNativeLauncherStatus(fd int, status byte) error {
	written, err := unix.Write(fd, []byte{status})
	if err != nil {
		return fmt.Errorf("write launcher status: %w", err)
	}
	if written != 1 {
		return fmt.Errorf("write launcher status: wrote %d bytes", written)
	}
	return nil
}

func installNativeNetworkSeccomp() error {
	architecture, ok := nativeAuditArchitecture()
	if !ok {
		return unix.ENOTSUP
	}
	filters, err := buildNativeNetworkSeccompFilters(architecture)
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

func buildNativeNetworkSeccompFilters(architecture uint32) ([]unix.SockFilter, error) {
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
	for _, syscallNumber := range nativeBlockedNetworkSyscalls() {
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(syscallNumber)}, //nolint:gosec // Linux syscall numbers are nonnegative uint32 values
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	filters = append(filters, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	return filters, nil
}

func nativeAuditArchitecture() (uint32, bool) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, true
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, true
	default:
		return 0, false
	}
}

func nativeBlockedNetworkSyscalls() []uintptr {
	return []uintptr{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR,
		unix.SYS_CONNECT, unix.SYS_BIND, unix.SYS_LISTEN, unix.SYS_ACCEPT, unix.SYS_ACCEPT4,
		unix.SYS_SENDTO, unix.SYS_SENDMSG, unix.SYS_SENDMMSG,
		unix.SYS_RECVFROM, unix.SYS_RECVMSG, unix.SYS_RECVMMSG,
		unix.SYS_SHUTDOWN, unix.SYS_GETSOCKNAME, unix.SYS_GETPEERNAME,
		unix.SYS_SETSOCKOPT, unix.SYS_GETSOCKOPT,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	}
}
