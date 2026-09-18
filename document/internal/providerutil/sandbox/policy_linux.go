//go:build linux

package sandbox

import (
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

const nativeX32SyscallBit = uint32(0x40000000)

func rootPath(parts ...string) string {
	values := append([]string{string(filepath.Separator)}, parts...)
	return filepath.Join(values...)
}

func execLandlockPaths() []string {
	return []string{
		rootPath("usr"), rootPath("lib"), rootPath("lib64"),
		rootPath("bin"), rootPath("proc"),
	}
}

func execLandlockFiles() []string {
	return []string{
		rootPath("etc", "ld.so.cache"),
		rootPath("etc", "localtime"),
	}
}

func execLandlockDevices() map[string]uint64 {
	return map[string]uint64{
		rootPath("dev", "null"):    unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE,
		rootPath("dev", "urandom"): unix.LANDLOCK_ACCESS_FS_READ_FILE,
		rootPath("dev", "zero"):    unix.LANDLOCK_ACCESS_FS_READ_FILE,
	}
}

func privateRootLandlockPaths() []string {
	return []string{
		rootPath("bin"), rootPath("dev"), rootPath("etc"), rootPath("lib"),
		rootPath("lib64"), rootPath("proc"), rootPath("usr"),
	}
}

func privateRootLandlockFiles() []string {
	return nil
}

func privateRootLandlockDevices() map[string]uint64 {
	return map[string]uint64{
		rootPath("dev", "null"):    unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE,
		rootPath("dev", "urandom"): unix.LANDLOCK_ACCESS_FS_READ_FILE,
		rootPath("dev", "zero"):    unix.LANDLOCK_ACCESS_FS_READ_FILE,
	}
}

func blockedNetworkSyscalls() []uintptr {
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

func execNetworkFilters(architecture uint32) ([]unix.SockFilter, error) {
	if architecture != unix.AUDIT_ARCH_X86_64 && architecture != unix.AUDIT_ARCH_AARCH64 {
		return nil, unix.ENOTSUP
	}
	filters := seccompPrefix(architecture)
	for _, syscallNumber := range blockedNetworkSyscalls() {
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(syscallNumber)}, //nolint:gosec
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	return append(filters, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW}), nil
}

func unixOnlyNetworkFilters(architecture uint32) ([]unix.SockFilter, error) {
	if architecture != unix.AUDIT_ARCH_X86_64 && architecture != unix.AUDIT_ARCH_AARCH64 {
		return nil, unix.ENOTSUP
	}
	filters := seccompPrefix(architecture)
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
	for _, syscallNumber := range []uintptr{
		unix.SYS_SENDTO, unix.SYS_SENDMSG, unix.SYS_SENDMMSG,
		unix.SYS_RECVFROM, unix.SYS_RECVMSG, unix.SYS_RECVMMSG,
		unix.SYS_SHUTDOWN, unix.SYS_GETSOCKNAME, unix.SYS_GETPEERNAME,
		unix.SYS_SETSOCKOPT, unix.SYS_GETSOCKOPT,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	} {
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: uint32(syscallNumber)}, //nolint:gosec
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	return append(filters, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW}), nil
}

func seccompPrefix(architecture uint32) []unix.SockFilter {
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
	return filters
}
