//go:build linux

package trafilatura

import (
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	nativeLauncherMarker          = "--docbank-internal-trafilatura-launch-v2"
	nativeLauncherExecutableFD    = 3
	nativeLauncherControlFD       = 4
	nativeLauncherStatusFD        = 5
	nativeLauncherTokenBytes      = 32
	nativeLauncherFailureExitCode = 125
	nativeLauncherReadyStatus     = byte(1)
	nativeLauncherFailureStatus   = byte(2)
	nativeX32SyscallBit           = uint32(0x40000000)
)

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
	if err := unix.Mount("proc", "/proc", "proc",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount private proc: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges: %w", err)
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
